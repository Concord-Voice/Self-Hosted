FROM postgres:16-alpine

RUN mkdir -p /etc/postgresql \
    && chown postgres:postgres /etc/postgresql \
    && chmod 0750 /etc/postgresql

COPY entrypoint-wrapper.sh configure-ops-metrics-hba.sh /usr/local/bin/
COPY init/00-ops-metrics-hba.sh /docker-entrypoint-initdb.d/
RUN chmod +x \
    /usr/local/bin/entrypoint-wrapper.sh \
    /usr/local/bin/configure-ops-metrics-hba.sh \
    /docker-entrypoint-initdb.d/00-ops-metrics-hba.sh

# The official postgres:16-alpine image drops privileges internally via `gosu postgres` inside
# docker-entrypoint.sh. Setting USER postgres before the ENTRYPOINT would prevent
# entrypoint-wrapper.sh from running the necessary `chown` on the WAL archive volume,
# since that operation requires root. Privilege dropping is handled by the upstream image.
# nosemgrep: dockerfile.security.missing-user-entrypoint.missing-user-entrypoint
ENTRYPOINT ["entrypoint-wrapper.sh"]
# nosemgrep: dockerfile.security.missing-user.missing-user
CMD ["postgres"]
