FROM gcr.io/distroless/static:nonroot
COPY runtime /runtime
USER 65532:65532
ENTRYPOINT ["/runtime"]
