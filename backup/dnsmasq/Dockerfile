# Plain dnsmasq. Not jpillora/dnsmasq: its webproc config editor listens on
# 8080, which would be reachable from the tailnet and by shared users here.
FROM alpine:3.20
RUN apk add --no-cache dnsmasq
ENTRYPOINT ["dnsmasq", "--keep-in-foreground", "--log-facility=-"]
