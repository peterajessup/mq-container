# © Copyright IBM Corporation 2015, 2019
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG BASE_IMAGE=registry.redhat.io/ubi8/ubi-minimal
ARG BASE_TAG=8.1-279

FROM $BASE_IMAGE:$BASE_TAG AS mq-server
# The MQ packages to install - see install-mq.sh for default value
ARG MQ_URL="https://public.dhe.ibm.com/ibmdl/export/pub/software/websphere/messaging/mqadv/mqadv_dev914_linux_x86-64.tar.gz"
ARG MQ_PACKAGES="MQSeriesRuntime-*.rpm MQSeriesServer-*.rpm MQSeriesJava*.rpm MQSeriesJRE*.rpm MQSeriesGSKit*.rpm MQSeriesMsg*.rpm MQSeriesSamples*.rpm MQSeriesWeb*.rpm MQSeriesAMS-*.rpm MQSeriesAMQP*.rpm"
#ARG MQ_PACKAGES="ibmmq-server ibmmq-java ibmmq-jre ibmmq-gskit ibmmq-msg-.* ibmmq-samples ibmmq-web ibmmq-ams"
ARG MQM_UID=888
ARG BASE_IMAGE
ARG BASE_TAG
LABEL summary="IBM MQ Advanced Server"
LABEL description="Simplify, accelerate and facilitate the reliable exchange of data with a security-rich messaging solution — trusted by the world’s most successful enterprises"
LABEL vendor="IBM"
LABEL distribution-scope="private"
LABEL authoritative-source-url="https://www.ibm.com/software/passportadvantage/"
LABEL url="https://www.ibm.com/products/mq/advanced"
LABEL io.openshift.tags="mq messaging"
LABEL io.k8s.display-name="IBM MQ Advanced Server"
LABEL io.k8s.description="Simplify, accelerate and facilitate the reliable exchange of data with a security-rich messaging solution — trusted by the world’s most successful enterprises"
LABEL base-image=$BASE_IMAGE
LABEL base-image-release=$BASE_TAG
COPY install-mq.sh /usr/local/bin/
COPY install-mq-server-prereqs.sh /usr/local/bin/
# Install MQ.  To avoid a "text file busy" error here, we sleep before installing.
RUN env && chmod u+x /usr/local/bin/install-*.sh \
  && sleep 1 \
  && install-mq-server-prereqs.sh $MQM_UID \
  && install-mq.sh $MQM_UID
# Create a directory for runtime data from runmqserver
RUN mkdir -p /run/runmqserver \
  && chown mqm:mqm /run/runmqserver
COPY --from=builder /opt/app-root/src/go/src/github.com/ibm-messaging/mq-container/runmqserver /usr/local/bin/
COPY --from=builder /opt/app-root/src/go/src/github.com/ibm-messaging/mq-container/chkmq* /usr/local/bin/
COPY NOTICES.txt /opt/mqm/licenses/notices-container.txt
# Copy web XML files
COPY web /etc/mqm/web
COPY etc/mqm/*.tpl /etc/mqm/
RUN chmod ug+x /usr/local/bin/runmqserver \
  && chown mqm:mqm /usr/local/bin/*mq* \
  && chmod ug+xs /usr/local/bin/chkmq* \
  && chown -R mqm:mqm /etc/mqm/* \
  && install --directory --mode 0775 --owner mqm --group root /run/runmqserver \
  && touch /run/termination-log \
  && chown mqm:root /run/termination-log \
  && chmod 0660 /run/termination-log
# Always use port 1414 for MQ & 9157 for the metrics
EXPOSE 1414 9157 9443
ENV LANG=en_US.UTF-8 AMQ_DIAGNOSTIC_MSG_SEVERITY=1 AMQ_ADDITIONAL_JSON_LOG=1 LOG_FORMAT=basic
USER $MQM_UID
ENTRYPOINT ["runmqserver"]

