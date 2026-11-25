# Image for the lhtx agent (lhtx agent --help); built by goreleaser
# (dockers_v2), which puts the binaries under $TARGETPLATFORM/.
FROM scratch
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/lhtx /lhtx
USER 65532:65532
ENTRYPOINT ["/lhtx"]
