# ---------------------------------------------------------------------------
# bore-tunnel — one container, many bore TCP tunnels.
#
# Stock ekzhang/bore is FROM scratch (just the static /bore binary, no shell),
# and `bore local` serves exactly one port per process. To publish several
# in-stack ports through a single service we drop the binary onto a tiny alpine
# base and let entrypoint.sh fan out one `bore local` per TUNNELS entry.
# ---------------------------------------------------------------------------
FROM ekzhang/bore AS bore

FROM alpine:3.20
# The bore binary is statically linked, so it runs on alpine unchanged.
COPY --from=bore /bore /usr/local/bin/bore
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
