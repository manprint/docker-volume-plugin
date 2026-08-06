# Builds the plugin rootfs image. Run `make build` first to produce bin/dvol-plugin.
FROM scratch
COPY bin/dvol-plugin /dvol-plugin
ENTRYPOINT ["/dvol-plugin"]