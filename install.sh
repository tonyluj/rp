#!/bin/bash

RP_DIR=/home/service/rp
RP_BINARY_FILE=${RP_DIR}/rp
RP_TOML_FILE=${RP_DIR}/rp.toml
RP_SERVICE_FILE=${RP_DIR}/rp.service
SYSTEMD_SERVICE=/etc/systemd/system/rp.service

role="client"

echo "===== RP CONFIG ====="
echo "rp working dir: ${RP_DIR}"
echo "rp toml file: ${RP_TOML_FILE}"
echo "rp systemd service source: ${RP_SERVICE_FILE}"
echo "rp systemd service target: ${SYSTEMD_SERVICE}"
echo "===== RP CONFIG ====="

read -p "While role do you want to run? [client,server] " -r role
if [[ ${role} = "client" ]]
then
  sed -i 's/role = "server"/role = "client"/g' ${RP_TOML_FILE}
elif [[ ${role} = "server" ]]
then
  sed -i 's/role = "client"/role = "server"/g' ${RP_TOML_FILE}
else
  echo "unknown role, exit."
  exit 100
fi

if test -f "${SYSTEMD_SERVICE}"; then
  read -p "${SYSTEMD_SERVICE} existed, do you want to replace? [y,n] " -n 1 -r
  echo
  if [[ ! $REPLY =~ ^[Yy]$ ]]
  then
    echo "not replace, exit."
    exit 100
  else
    echo "replace ${SYSTEMD_SERVICE} with ${RP_SERVICE_FILE}."
  fi
fi

cp ${RP_SERVICE_FILE} ${SYSTEMD_SERVICE}
ret=$?
if [ ${ret} -ne 0 ]
then
  echo "copy file failed, exit."
  exit 100
fi

echo "copy ${RP_SERVICE_FILE} to ${SYSTEMD_SERVICE} done."
