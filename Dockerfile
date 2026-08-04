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

ARG SPARK_IMAGE=onehouse/spark-3.5.3-base:130825

FROM golang:1.23.12 AS builder

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

ARG SPARK_UID=185

ARG SPARK_GID=185

USER root

# Install dependencies and add JARs in optimized layers
RUN set -ex; \
    # Install tini
    apt-get update && \
    apt-get install -y --no-install-recommends tini wget && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    # Create directories
    mkdir -p $SPARK_HOME/jars /etc/k8s-webhook-server/serving-certs /home/spark && \
    # Download all JARs in parallel for better performance
    wget -q -O $SPARK_HOME/jars/hadoop-aws-3.1.1.jar \
        https://repo1.maven.org/maven2/org/apache/hadoop/hadoop-aws/3.1.1/hadoop-aws-3.1.1.jar && \
    wget -q -O $SPARK_HOME/jars/aws-java-sdk-bundle-1.11.814.jar \
        https://repo1.maven.org/maven2/com/amazonaws/aws-java-sdk-bundle/1.11.814/aws-java-sdk-bundle-1.11.814.jar && \
    # NOTE (ENG-46085): spark-avro is pinned to 3.5.2 further down, matching the Spark
    # 3.5.3 runtime in this image and the version jobs actually request. The old 3.1.1
    # copy is NOT fetched — two spark-avro versions on one classpath is a
    # ClassCastException waiting to happen, and 3.1.1 never matched this runtime.
    wget -q -O $SPARK_HOME/jars/gcs-connector-hadoop3-latest.jar \
        https://storage.googleapis.com/hadoop-lib/gcs/gcs-connector-hadoop3-latest.jar && \
    wget -q -O $SPARK_HOME/jars/hadoop-azure-3.3.4.jar \
        https://repo1.maven.org/maven2/org/apache/hadoop/hadoop-azure/3.3.4/hadoop-azure-3.3.4.jar && \
    wget -q -O $SPARK_HOME/jars/azure-storage-blob-12.25.0.jar \
        https://repo1.maven.org/maven2/com/azure/azure-storage-blob/12.25.0/azure-storage-blob-12.25.0.jar && \
    wget -q -O $SPARK_HOME/jars/azure-core-1.51.0.jar \
        https://repo1.maven.org/maven2/com/azure/azure-core/1.51.0/azure-core-1.51.0.jar && \
    wget -q -O $SPARK_HOME/jars/azure-core-http-netty-1.15.3.jar \
        https://repo1.maven.org/maven2/com/azure/azure-core-http-netty/1.15.3/azure-core-http-netty-1.15.3.jar && \
    # Structured-streaming Kafka dependency closure (ENG-46085).
    #
    # spark-submit runs INSIDE this image, and Ivy resolves --packages into a single
    # shared cache (default /tmp/.ivy2). Concurrent submits therefore race on the
    # <artifact>.part -> <artifact> rename and read each other's partial files back as
    # 0 bytes, failing the submit before any driver pod exists. Baking the closure here
    # removes the resolution step for these coordinates entirely, so there is nothing
    # to race on.
    #
    # This is the CLOSURE Ivy resolves for
    # `--packages org.apache.spark:spark-sql-kafka-0-10_2.12:3.5.2,org.apache.spark:spark-avro_2.12:3.5.2`
    # MINUS the 8 members Spark 3.5.3's own jars/ already ships at identical versions
    # (hadoop-client-api/runtime 3.3.4, lz4-java 1.8.0, snappy-java 1.1.10.5,
    # slf4j-api 2.0.7, commons-logging 1.1.3, jsr305 3.0.0, xz 1.9) — those were being
    # needlessly re-downloaded on every run and are the artifacts that actually
    # collided in production. Versions below are pinned to what Spark 3.5.3 resolves;
    # bump them together with SPARK_TGZ_URL in spark-3.5.3-base/Dockerfile.
    wget -q -O $SPARK_HOME/jars/spark-sql-kafka-0-10_2.12-3.5.2.jar \
        https://repo1.maven.org/maven2/org/apache/spark/spark-sql-kafka-0-10_2.12/3.5.2/spark-sql-kafka-0-10_2.12-3.5.2.jar && \
    wget -q -O $SPARK_HOME/jars/spark-token-provider-kafka-0-10_2.12-3.5.2.jar \
        https://repo1.maven.org/maven2/org/apache/spark/spark-token-provider-kafka-0-10_2.12/3.5.2/spark-token-provider-kafka-0-10_2.12-3.5.2.jar && \
    wget -q -O $SPARK_HOME/jars/kafka-clients-3.4.1.jar \
        https://repo1.maven.org/maven2/org/apache/kafka/kafka-clients/3.4.1/kafka-clients-3.4.1.jar && \
    wget -q -O $SPARK_HOME/jars/commons-pool2-2.11.1.jar \
        https://repo1.maven.org/maven2/org/apache/commons/commons-pool2/2.11.1/commons-pool2-2.11.1.jar && \
    wget -q -O $SPARK_HOME/jars/spark-avro_2.12-3.5.2.jar \
        https://repo1.maven.org/maven2/org/apache/spark/spark-avro_2.12/3.5.2/spark-avro_2.12-3.5.2.jar && \
    # Set permissions for all JARs at once
    chmod 644 $SPARK_HOME/jars/*.jar && \
    # Set directory permissions
    chmod -R g+rw /etc/k8s-webhook-server/serving-certs && \
    chown -R spark /etc/k8s-webhook-server/serving-certs /home/spark

USER ${SPARK_UID}:${SPARK_GID}

COPY --from=builder /workspace/bin/spark-operator /usr/bin/spark-operator

COPY entrypoint.sh /usr/bin/

ENTRYPOINT ["/usr/bin/entrypoint.sh"]
