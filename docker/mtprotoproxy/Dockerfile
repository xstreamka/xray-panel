FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/mtprotoproxy
RUN git clone --depth 1 --branch stable https://github.com/alexbers/mtprotoproxy.git /tmp/mtprotoproxy \
    && cp /tmp/mtprotoproxy/mtprotoproxy.py /opt/mtprotoproxy/mtprotoproxy.py \
    && rm -rf /tmp/mtprotoproxy

RUN pip install --no-cache-dir cryptography PySocks uvloop

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /config
ENTRYPOINT ["/entrypoint.sh"]
