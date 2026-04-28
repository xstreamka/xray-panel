FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# Пинимся к конкретному коммиту upstream'а, чтобы pull/rebuild не притаскивал
# чужие изменения (или скомпрометированные апстримом). Когда захотим обновить,
# меняем SHA здесь явно — после ручного аудита diff'а.
ARG MTPROTOPROXY_SHA=0614c35020943b2080c9bd27b5e6336270af389f

WORKDIR /opt/mtprotoproxy
RUN git clone https://github.com/alexbers/mtprotoproxy.git /tmp/mtprotoproxy \
    && cd /tmp/mtprotoproxy \
    && git checkout "${MTPROTOPROXY_SHA}" \
    && cp mtprotoproxy.py /opt/mtprotoproxy/mtprotoproxy.py \
    && rm -rf /tmp/mtprotoproxy

# Версии pinned, чтобы транзитивные зависимости не двигались без спросa.
RUN pip install --no-cache-dir \
        cryptography==44.0.0 \
        PySocks==1.7.1 \
        uvloop==0.21.0

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /config
ENTRYPOINT ["/entrypoint.sh"]
