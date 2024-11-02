#
# Copyright 2017 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

ARG SPARK_IMAGE=spark:3.5.2

FROM golang:1.23.1 AS builder

WORKDIR /workspace

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download

COPY . .
ENV GOCACHE=/root/.cache/go-build
ARG TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GO111MODULE=on make build-operator

FROM ${SPARK_IMAGE}

USER root

# Add AWS Jars
ADD https://repo1.maven.org/maven2/org/apache/hadoop/hadoop-aws/3.3.4/hadoop-aws-3.3.4.jar $SPARK_HOME/jars
RUN chmod 644 $SPARK_HOME/jars/hadoop-aws-3.3.4.jar

ADD https://repo1.maven.org/maven2/com/amazonaws/aws-java-sdk-bundle/1.12.691/aws-java-sdk-bundle-1.12.691.jar $SPARK_HOME/jars
RUN chmod 644 $SPARK_HOME/jars/aws-java-sdk-bundle-1.11.814.jar

ADD https://repo1.maven.org/maven2/org/apache/spark/spark-avro_2.13/3.3.4/spark-avro_2.13-3.3.4.jar $SPARK_HOME/jars
RUN chmod 644 $SPARK_HOME/jars/spark-avro_2.12-3.1.1.jar

# Add gcs connector
ADD https://storage.googleapis.com/hadoop-lib/gcs/gcs-connector-hadoop3-latest.jar  $SPARK_HOME/jars
RUN chmod 644 $SPARK_HOME/jars/gcs-connector-hadoop3-latest.jar

COPY --from=builder /usr/bin/spark-operator /usr/bin/
RUN apt-get update --allow-releaseinfo-change \
    && apt-get update \
    && apt-get install -y openssl curl tini \
    && rm -rf /var/lib/apt/lists/*

COPY hack/gencerts.sh /usr/bin/

COPY entrypoint.sh /usr/bin/

ENTRYPOINT ["/usr/bin/entrypoint.sh"]
