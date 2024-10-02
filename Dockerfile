FROM golang:latest
RUN go install github.com/go-delve/delve/cmd/dlv@latest

FROM golang:latest

WORKDIR /src

COPY ./yb-voyager/go.mod ./yb-voyager/go.sum ./
RUN go mod download

COPY ./yb-voyager/ ./

RUN go build


FROM amd64/ubuntu:22.04
ARG DEBIAN_FRONTEND=noninteractive
ENV TZ=UTC
ENV LANG en_US.UTF-8
ENV LANGUAGE en_US:en
ENV LC_ALL en_US.UTF-8
RUN apt-get update && apt-get install -y curl perl make libdbi-perl locales && \
    curl -L https://cpanmin.us | perl - --self-upgrade && \
    ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone && \
    apt-get install -y tzdata wget rsync openjdk-17-jre git sudo gcc jq && \
    apt-get upgrade -y binutils && \
    curl -sL https://aka.ms/InstallAzureCLIDeb | bash && \
    git clone https://github.com/yugabyte/yb-voyager.git && \
    cd yb-voyager/installer_scripts && \
    locale-gen en_US.UTF-8 && \
    yes | ./install-yb-voyager && \
    . ~/.yb-voyager.rc && \
    apt-get remove -y gcc curl make wget git && \
    rm -rf /yb-voyager /usr/share/icons /usr/share/fonts /usr/share/doc && \
    rm -rf /root/go /root/.cache /tmp/go && \
    apt-get clean
COPY --from=0 $HOME/go/bin/dlv /usr/local/bin
COPY --from=1 /src/yb-voyager /usr/local/bin
CMD [“yb-voyager”]
