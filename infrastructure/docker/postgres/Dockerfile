FROM postgres:16-alpine

COPY entrypoint-wrapper.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/entrypoint-wrapper.sh

# The official postgres:16-alpine image drops privileges internally via `gosu postgres` inside
# docker-entrypoint.sh. Setting USER postgres before the ENTRYPOINT would prevent
# entrypoint-wrapper.sh from running the necessary `chown` on the WAL archive volume,
# since that operation requires root. Privilege dropping is handled by the upstream image.
# nosemgrep: dockerfile.security.missing-user-entrypoint.missing-user-entrypoint
ENTRYPOINT ["entrypoint-wrapper.sh"]
# nosemgrep: dockerfile.security.missing-user.missing-user
CMD ["postgres"]
