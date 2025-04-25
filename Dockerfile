FROM ubuntu:24.04

# Install build dependencies including Go from Ubuntu package
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    build-essential \
    pkg-config \
    # Go 1.23 from Ubuntu package
    golang-1.23-go \
    # GStreamer development packages
    libgstreamer1.0-dev \
    libgstreamer-plugins-base1.0-dev \
    gstreamer1.0-plugins-base \
    gstreamer1.0-plugins-good \
    gstreamer1.0-plugins-bad \
    gstreamer1.0-plugins-ugly \
    gstreamer1.0-libav \
    gstreamer1.0-tools \
    gstreamer1.0-x \
    gstreamer1.0-alsa \
    gstreamer1.0-gl \
    gstreamer1.0-gtk3 \
    gstreamer1.0-qt5 \
    gstreamer1.0-pulseaudio \
    gstreamer1.0-nice \
    gstreamer1.0-python3-plugin-loader \
    gstreamer1.0-rtsp \
    gstreamer1.0-vaapi \
    # Additional codec support
    gstreamer1.0-plugins-base-apps \
    # Dependencies for specific elements
    ffmpeg \
    libavcodec-extra \
    libavfilter-extra \
    # Support for various formats
    libx264-dev \
    libx265-dev \
    libvpx-dev \
    libopus-dev \
    # Certificates
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Set up Go environment (Ubuntu package puts Go in /usr/lib/go-1.23)
ENV PATH=$PATH:/usr/lib/go-1.23/bin
ENV GOPATH=/go
ENV PATH=$PATH:$GOPATH/bin

# Set environment variables to help GStreamer find all plugins
ENV GST_PLUGIN_PATH=/usr/lib/x86_64-linux-gnu/gstreamer-1.0
ENV GST_PLUGIN_SYSTEM_PATH=/usr/lib/x86_64-linux-gnu/gstreamer-1.0
ENV GST_REGISTRY_UPDATE=yes

# Build the application
WORKDIR /app
COPY . .
RUN go build -o /usr/local/bin/gstreamer-publisher

# Create a non-root user to run the application
RUN useradd -m appuser
USER appuser
WORKDIR /home/appuser

ENTRYPOINT ["gstreamer-publisher"]