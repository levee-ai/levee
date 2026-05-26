FROM alpine:3.20 AS base
RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -u 1000 levee

FROM scratch
COPY --from=base /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=base /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=base /etc/passwd /etc/passwd
COPY levee /usr/local/bin/levee

USER levee
ENTRYPOINT ["levee"]
CMD ["serve", "--config", "/etc/levee/config.yaml"]
