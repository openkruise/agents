FROM chromedp/headless-shell

RUN find /etc/apt/sources.list.d/ -name "*.list" -type f -exec sed -i 's/archive.ubuntu.com/mirrors.aliyun.com/g' {} \; && \
    find /etc/apt/sources.list.d/ -name "*.list" -type f -exec sed -i 's/security.ubuntu.com/mirrors.aliyun.com/g' {} \; && \
    find /etc/apt/sources.list.d/ -name "*.sources" -type f -exec sed -i 's/deb.debian.org/mirrors.aliyun.com/g' {} \; && \
    find /etc/apt/sources.list.d/ -name "*.sources" -type f -exec sed -i 's/security.debian.org/mirrors.aliyun.com/g' {} \;

RUN apt-get update && apt-get install -y \
  xfonts-intl-chinese \
  ttf-wqy-microhei \
  xfonts-wqy \
  && rm -rf /var/lib/apt/lists/*