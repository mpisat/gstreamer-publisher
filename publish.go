// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	supportedAudioMimeTypes = []string{
		"audio/x-opus",
	}
	supportedVideoMimeTypes = []string{
		"video/x-h264",
		"video/x-vp8",
		"video/x-vp9",
		"video/x-av1",
	}
)

type PublisherParams struct {
	URL            string
	Token          string
	PipelineString string
}

type Publisher struct {
	params     PublisherParams
	pipeline   *gst.Pipeline
	loop       *glib.MainLoop
	videoTrack *publisherTrack
	audioTrack *publisherTrack
	room       *lksdk.Room

	// For managing shutdown
	shutdownCh   chan struct{}
	restartCh    chan struct{}
	mu           sync.Mutex
	shuttingDown bool

	// Track reconnection attempts
	reconnecting atomic.Bool
}

type elementTarget struct {
	element  *gst.Element
	srcPad   *gst.Pad
	mimeType string
	isAudio  bool
}

func NewPublisher(params PublisherParams) *Publisher {
	return &Publisher{
		params:     params,
		shutdownCh: make(chan struct{}),
		restartCh:  make(chan struct{}, 1),
	}
}

func (p *Publisher) Start() error {
	// Set up signal handling once for the lifetime of the program
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sigChan
		p.mu.Lock()
		p.shuttingDown = true
		p.mu.Unlock()
		close(p.shutdownCh)
		p.Stop()
		os.Exit(0)
	}()

	// Main reconnection loop
	for {
		logger.Infow("starting publisher session")

		// Initialize everything from scratch
		if err := p.initialize(); err != nil {
			logger.Infow("initialization failed, retrying in 3 seconds", "error", err)
			time.Sleep(3 * time.Second)
			continue
		}

		// Set up room connection with callbacks
		cb := lksdk.NewRoomCallback()

		// Handle explicit disconnections
		cb.OnDisconnected = func() {
			logger.Infow("disconnected from LiveKit, triggering restart")
			select {
			case p.restartCh <- struct{}{}:
			default:
				// Already has a pending restart
			}
		}

		// Handle reconnection attempts
		cb.OnReconnecting = func() {
			logger.Infow("reconnecting to LiveKit, tracking reconnection state")
			p.reconnecting.Store(true)
		}

		// When reconnected, we'll also trigger a restart to ensure tracks are republished
		cb.OnReconnected = func() {
			if p.reconnecting.Load() {
				logger.Infow("reconnected to LiveKit, forcing a full restart to republish tracks")
				p.reconnecting.Store(false)
				select {
				case p.restartCh <- struct{}{}:
				default:
					// Already has a pending restart
				}
			}
		}

		p.room = lksdk.NewRoom(cb)
		err := p.room.JoinWithToken(p.params.URL, p.params.Token,
			lksdk.WithAutoSubscribe(false),
		)
		if err != nil {
			logger.Infow("room join failed, retrying in 3 seconds", "error", err)
			p.cleanup()
			time.Sleep(3 * time.Second)
			continue
		}

		// Track that we've successfully connected
		logger.Infow("successfully connected to LiveKit room",
			"roomID", p.room.SID(),
			"participantID", p.room.LocalParticipant.SID())

		// Publish tracks
		if p.videoTrack != nil {
			pub, err := p.room.LocalParticipant.PublishTrack(p.videoTrack.track, &lksdk.TrackPublicationOptions{
				Source: livekit.TrackSource_CAMERA,
			})
			if err != nil {
				logger.Infow("failed to publish video track, retrying session", "error", err)
				p.cleanup()
				time.Sleep(3 * time.Second)
				continue
			}
			p.videoTrack.publication = pub
			logger.Infow("published video track", "trackID", pub.SID())
			p.videoTrack.onEOS = func() {
				_ = p.room.LocalParticipant.UnpublishTrack(pub.SID())
			}
		}

		if p.audioTrack != nil {
			pub, err := p.room.LocalParticipant.PublishTrack(p.audioTrack.track, &lksdk.TrackPublicationOptions{
				Source: livekit.TrackSource_MICROPHONE,
			})
			if err != nil {
				logger.Infow("failed to publish audio track, retrying session", "error", err)
				p.cleanup()
				time.Sleep(3 * time.Second)
				continue
			}
			p.audioTrack.publication = pub
			logger.Infow("published audio track", "trackID", pub.SID())
			p.audioTrack.onEOS = func() {
				_ = p.room.LocalParticipant.UnpublishTrack(pub.SID())
			}
		}

		// Start the pipeline
		if err := p.pipeline.Start(); err != nil {
			logger.Infow("failed to start pipeline, retrying", "error", err)
			p.cleanup()
			time.Sleep(3 * time.Second)
			continue
		}

		// Set up watchdog to monitor connection status
		go p.startConnectionWatchdog()

		// Start a goroutine to watch for restart signals
		restartComplete := make(chan struct{})
		go func() {
			select {
			case <-p.restartCh:
				logger.Infow("restart requested, stopping current session")
				// First, try to cleanly disconnect from the room
				if p.room != nil {
					p.room.Disconnect()
				}
				// Then, perform full cleanup
				p.cleanup()
				close(restartComplete)
			case <-p.shutdownCh:
				// Just exit the goroutine on shutdown
			}
		}()

		// Run the main loop until it exits
		p.loop.Run()

		// Wait for restart to complete if it was triggered
		<-restartComplete

		// Check if we're shutting down
		p.mu.Lock()
		shuttingDown := p.shuttingDown
		p.mu.Unlock()
		if shuttingDown {
			return nil
		}

		// Wait before reconnecting
		logger.Infow("session ended, reconnecting in 3 seconds")
		time.Sleep(3 * time.Second)
	}
}

// Periodically check connection status as a backup detection mechanism
func (p *Publisher) startConnectionWatchdog() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// If reconnection is in progress, check if we need to trigger restart
			if p.reconnecting.Load() {
				logger.Infow("connection still in reconnecting state, forcing restart")
				select {
				case p.restartCh <- struct{}{}:
					p.reconnecting.Store(false)
				default:
					// Already has a pending restart
				}
			}

			// Check if room connection is active
			// Instead of checking connection quality (which isn't available),
			// check if we've been in reconnecting state for too long
			if p.room != nil && p.reconnecting.Load() {
				logger.Infow("detected prolonged reconnection state, triggering restart")
				select {
				case p.restartCh <- struct{}{}:
				default:
					// Already has a pending restart
				}
			}
		case <-p.shutdownCh:
			return
		}
	}
}

func (p *Publisher) cleanup() {
	logger.Infow("performing cleanup")
	// Make sure to shutdown pipeline first
	if p.pipeline != nil {
		logger.Infow("setting pipeline to NULL state")
		p.pipeline.BlockSetState(gst.StateNull)
		p.pipeline = nil
	}

	// Unpublish tracks
	if p.room != nil && p.room.LocalParticipant != nil {
		if p.videoTrack != nil && p.videoTrack.publication != nil {
			logger.Infow("unpublishing video track",
				"trackID", p.videoTrack.publication.SID())
			_ = p.room.LocalParticipant.UnpublishTrack(p.videoTrack.publication.SID())
		}
		if p.audioTrack != nil && p.audioTrack.publication != nil {
			logger.Infow("unpublishing audio track",
				"trackID", p.audioTrack.publication.SID())
			_ = p.room.LocalParticipant.UnpublishTrack(p.audioTrack.publication.SID())
		}
	}

	// Disconnect room connection
	if p.room != nil {
		logger.Infow("disconnecting from LiveKit room")
		p.room.Disconnect()
		p.room = nil
	}

	// Quit main loop
	if p.loop != nil {
		logger.Infow("quitting main loop")
		p.loop.Quit()
		p.loop = nil
	}

	// Clear track references
	p.videoTrack = nil
	p.audioTrack = nil

	// Reset reconnection state
	p.reconnecting.Store(false)

	logger.Infow("cleanup complete")
}

func (p *Publisher) Stop() {
	logger.Infow("stopping publisher..")
	p.cleanup()
}

func (p *Publisher) messageWatch(msg *gst.Message) bool {
	switch msg.Type() {
	case gst.MessageEOS:
		logger.Infow("EOS received, looping pipeline")
		// Seek to the start of the file
		success := p.pipeline.SeekSimple(0, gst.FormatTime, gst.SeekFlagFlush|gst.SeekFlagKeyUnit)
		if !success {
			logger.Infow("Failed to seek pipeline to start, triggering restart")
			// Trigger restart
			select {
			case p.restartCh <- struct{}{}:
			default:
				// Channel already has a pending restart
			}
			return false
		}
		// Reset track ended states to prevent unpublishing
		if p.videoTrack != nil {
			p.videoTrack.isEnded.Store(false)
		}
		if p.audioTrack != nil {
			p.audioTrack.isEnded.Store(false)
		}
		return true // Continue running the pipeline

	case gst.MessageError:
		gerr := msg.ParseError()
		logger.Infow("pipeline error, triggering restart", "error", gerr)
		// Trigger restart
		select {
		case p.restartCh <- struct{}{}:
		default:
			// Channel already has a pending restart
		}
		return false

	case gst.MessageTag, gst.MessageStateChanged, gst.MessageLatency, gst.MessageAsyncDone, gst.MessageStreamStatus, gst.MessageElement:
		// ignore

	default:
		logger.Debugw(msg.String())
	}

	return true
}

func (p *Publisher) initialize() error {
	gst.Init(nil)
	p.loop = glib.NewMainLoop(glib.MainContextDefault(), false)
	pipeline, err := gst.NewPipelineFromString(p.params.PipelineString)
	if err != nil {
		return err
	}
	pipeline.GetPipelineBus().AddWatch(p.messageWatch)

	// auto-find audio and video elements matching our specs
	targets, err := p.discoverSuitableElements(pipeline)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		return errors.New("no supported elements found. pipeline needs to include encoded audio or video")
	}

	for _, target := range targets {
		if p.videoTrack != nil && !target.isAudio {
			return errors.New("pipeline has more than one video source")
		} else if p.audioTrack != nil && target.isAudio {
			return errors.New("pipeline has more than one audio source")
		}
		pt, err := createPublisherTrack(target.mimeType)
		if err != nil {
			return err
		}
		if err := pipeline.Add(pt.sink.Element); err != nil {
			return err
		}
		if err := target.element.Link(pt.sink.Element); err != nil {
			return err
		}

		logger.Infow("found source", "mimeType", target.mimeType)
		if target.isAudio {
			p.audioTrack = pt
		} else {
			p.videoTrack = pt
		}
	}

	p.pipeline = pipeline
	return nil
}

func (p *Publisher) discoverSuitableElements(pipeline *gst.Pipeline) ([]elementTarget, error) {
	elements, err := pipeline.GetElements()
	if err != nil {
		return nil, err
	}

	var targets []elementTarget
	for _, e := range elements {
		pads, err := e.GetSrcPads()
		if err != nil {
			return nil, err
		}
		for _, pad := range pads {
			if !pad.IsLinked() {
				caps := pad.GetPadTemplateCaps()
				if caps == nil {
					continue
				}
				structure := caps.GetStructureAt(0)
				mime := structure.Name()
				if slices.Contains(supportedAudioMimeTypes, mime) {
					targets = append(targets, elementTarget{
						element:  e,
						srcPad:   pad,
						mimeType: mime,
						isAudio:  true,
					})
				} else if slices.Contains(supportedVideoMimeTypes, mime) {
					targets = append(targets, elementTarget{
						element:  e,
						srcPad:   pad,
						mimeType: mime,
						isAudio:  false,
					})
				}
			}
		}
	}
	return targets, nil
}
