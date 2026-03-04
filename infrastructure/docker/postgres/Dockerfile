FROM postgres:16-alpine

COPY entrypoint-wrapper.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/entrypoint-wrapper.sh

ENTRYPOINT ["entrypoint-wrapper.sh"]
CMD ["postgres"]
