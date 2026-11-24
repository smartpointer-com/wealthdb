# wealthdb/native-test — the tools a tree of the native engine needs, in
# one image, for `make test-native`: Go, Java, Python and the commands
# the tests call. It holds no wealthdb code. A run copies the tree in,
# configures it with --engine=native, and builds and tests it there, so
# the native engine is exercised without installing any of this on the
# computer.
#
# GO_IMAGE, JAVA_IMAGE and PYTHON_IMAGE are the images the engine, the
# dashboards and the Python collectors build FROM (wealthdb/Dockerfile,
# web/Dockerfile, shared/images/base-python.Dockerfile): the one place
# each version is declared. The Makefile passes them. Java and Python
# are copied out of theirs. The Go and Python images have to be built on
# one Debian release: the libraries installed below for the copied
# Python are that release's packages (libssl3, libffi8, libreadline8).
ARG GO_IMAGE
ARG JAVA_IMAGE
ARG PYTHON_IMAGE
FROM ${JAVA_IMAGE} AS java
FROM ${PYTHON_IMAGE} AS python

FROM ${GO_IMAGE}
# The commands the tests call, and the libraries Python loads.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl fontconfig fonts-dejavu-core git lsof make openssl procps \
      libbz2-1.0 libexpat1 libffi8 liblzma5 libncursesw6 libreadline8 libsqlite3-0 libssl3 \
      libuuid1 zlib1g && \
    rm -rf /var/lib/apt/lists/*
COPY --from=java /opt/java/openjdk /opt/java/openjdk
ENV JAVA_HOME=/opt/java/openjdk
COPY --from=python /usr/local /usr/local
# Fails the build, not a test, when the copied Python cannot load a
# library or is too old for tarfile's extraction filters.
RUN ldconfig && python3 -c 'import ssl, sqlite3, venv, tarfile; tarfile.data_filter'
# A run is not root, and works here.
RUN mkdir -m 1777 /work
WORKDIR /work
