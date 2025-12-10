FROM public.ecr.aws/lambda/provided:al2023

# GO and PHP versions
ARG GO_VERSION=1.25.5
ARG PHP_VERSION=8.4.15
ARG TARGETPLATFORM=linux/arm64
ARG TARGETOS=linux
ARG TARGETARCH=arm64

# Environmental Variables
ENV CGO_ENABLED=0
ENV COMPOSER_ALLOW_SUPERUSER=1
ENV GO111MODULE=on
ENV GOPATH=/root/go
ENV GOPROXY=https://proxy.golang.org,direct
ENV GOROOT=/usr/local/go
ENV PATH=$PATH:$GOROOT/bin:$GOPATH/bin:/usr/local/bin

# Install Dependencies
RUN dnf update -y
RUN dnf upgrade -y
RUN dnf install -y \
  autoconf \
  bison \
  bzip2 \
  bzip2-devel \
  freetype-devel \
  gcc \
  gcc-c++ \
  gzip \
  libcurl-devel \
  libffi-devel \
  libicu-devel \
  libjpeg-turbo-devel \
  libpng-devel \
  libsodium-devel \
  libtool \
  libwebp-devel \
  libxml2-devel \
  libzip-devel \
  make \
  oniguruma-devel \
  openssl-devel \
  pkgconfig \
  protobuf \
  protobuf-devel \
  re2c \
  sqlite-devel \
  tar \
  wget \
  zlib-devel

WORKDIR /tmp

# Install Go
RUN wget https://go.dev/dl/go${GO_VERSION}.${TARGETOS}-${TARGETARCH}.tar.gz && \
    tar -C /usr/local -xzf go${GO_VERSION}.${TARGETOS}-${TARGETARCH}.tar.gz && \
    rm go${GO_VERSION}.${TARGETOS}-${TARGETARCH}.tar.gz

# Install PHP
RUN wget https://www.php.net/distributions/php-${PHP_VERSION}.tar.gz && \
    tar -xzf php-${PHP_VERSION}.tar.gz && \
    rm php-${PHP_VERSION}.tar.gz
WORKDIR /tmp/php-${PHP_VERSION}
RUN ./configure --prefix=/usr/local \
    --enable-gd \
    --enable-intl \
    --enable-mbstring \
    --enable-opcache \
    --enable-option-checking=fatal \
    --enable-pcntl \
    --enable-sockets \
    --enable-xml \
    --with-config-file-path=/usr/local/etc \
    --with-config-file-scan-dir=/usr/local/etc/conf.d \
    --with-curl \
    --with-freetype \
    --with-jpeg \
    --with-libdir=lib64 \
    --with-mysqli=mysqlnd \
    --with-openssl \
    --with-pdo-mysql=mysqlnd \
    --with-pdo-sqlite \
    --with-pear \
    --with-sodium \
    --with-zip \
    --with-zlib && \
    make -j"$(nproc)" && \
    make install && \
    mkdir -p /usr/local/etc/conf.d && \
    cp php.ini-production /usr/local/etc/php.ini

WORKDIR /tmp

# Install PECL extensions, composer
RUN printf "\n" | pecl install protobuf && \
  echo "extension=protobuf.so" >> /usr/local/etc/conf.d/protobuf.ini && \
  echo "zend_extension=opcache" >> /usr/local/etc/conf.d/opcache.ini && \
  php -r "copy('https://getcomposer.org/installer', 'composer-setup.php');" && \
  php composer-setup.php --install-dir=/usr/local/bin --filename=composer && \
  rm composer-setup.php

# Install RoadRunner
RUN mkdir -p /tmp/roadrunner
WORKDIR /tmp/roadrunner
COPY --exclude="*_test.go" ./.golangci.yml ./*.go ./go.mod ./go.sum ./.rr.yaml ./
RUN go mod vendor
RUN go build -trimpath -ldflags "-s" -o bootstrap main.go plugin.go request_parser.go
RUN mv bootstrap /var/runtime/bootstrap
RUN rm -rf /tmp/*

WORKDIR /var/task
COPY \
  --exclude=.cache \
  --exclude=.git* \
	--exclude=*.go \
  --exclude=.golangci.yml \
  --exclude=vendor \
  --exclude=Dockerfile \
	. /var/task

# Handle Composer tasks
RUN set -eux; \
	composer dump-autoload --classmap-authoritative --no-dev; \
	composer dump-env prod; \
	chmod +x bin/console; \
	sync;

# Setup PHP configuration
COPY php-conf/* /usr/local/etc/php/conf.d/

ENTRYPOINT [ "/var/runtime/bootstrap" ]