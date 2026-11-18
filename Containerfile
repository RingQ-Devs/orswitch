# Build
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/orswitch ./cmd/orswitch \
 && CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/or_cli ./cmd/or_cli

# Run. Host networking is strongly recommended for SIP/RTP, e.g.:
#   <container tool> run --network host -v $PWD/conf:/etc/orswitch orswitch
FROM alpine:3.20
RUN adduser -D -H -s /sbin/nologin ringq \
 && mkdir -p /etc/orswitch /var/lib/orswitch/log /var/lib/orswitch/recordings /var/lib/orswitch/sounds \
 && chown -R ringq /var/lib/orswitch
COPY --from=build /out/orswitch /out/or_cli /usr/local/bin/
COPY conf /etc/orswitch
# The sample config uses paths relative to the conf dir; point them at the
# writable data directory instead.
RUN sed -i -e 's#"../log/cdr.csv"#"/var/lib/orswitch/log/cdr.csv"#' \
           -e 's#"../sounds"#"/var/lib/orswitch/sounds"#' \
           -e 's#"../recordings"#"/var/lib/orswitch/recordings"#' /etc/orswitch/orswitch.xml
USER ringq
EXPOSE 5060/udp 5060/tcp 16384-32768/udp
ENTRYPOINT ["/usr/local/bin/orswitch", "-conf", "/etc/orswitch"]
