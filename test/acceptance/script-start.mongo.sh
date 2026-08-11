#!/usr/bin/env bash
set -euo pipefail
 
# Percorsi sull'host (relativi alla cartella da cui lanci lo script)
BASE_DIR="$(pwd)/volumes"
DATA_DIR="$BASE_DIR/mongo_data"
LOGS_DIR="$BASE_DIR/mongo_logs"
 
# Crea le cartelle sul host se non esistono
mkdir -p "$DATA_DIR" "$LOGS_DIR"
 
# Permessi corretti per l'utente "mongodb" dentro il container (uid/gid 999)
if ! chown -R 999:999 "$DATA_DIR" "$LOGS_DIR" 2>/dev/null; then
  sudo chown -R 999:999 "$DATA_DIR" "$LOGS_DIR"
fi
 
# Crea i volumi Docker come bind mount "travestiti" da named volume
# (docker volume create è idempotente: se esistono già, non fa nulla)
docker volume create \
  --name mongo_data \
  --driver local \
  --opt type=none \
  --opt device="$DATA_DIR" \
  --opt o=bind
 
docker volume create \
  --name mongo_logs \
  --driver local \
  --opt type=none \
  --opt device="$LOGS_DIR" \
  --opt o=bind
 
# Rimuove un eventuale container precedente con lo stesso nome
docker rm -f mongodb 2>/dev/null || true
 
# Avvia il container MongoDB
docker run -d \
  --name mongodb \
  --restart unless-stopped \
  -p 27017:27017 \
  -e MONGO_INITDB_ROOT_USERNAME=admin \
  -e MONGO_INITDB_ROOT_PASSWORD=changeme \
  -v mongo_data:/data/db \
  -v mongo_logs:/var/log/mongodb \
  "${MONGO_IMAGE:-mongo:7.0.39-jammy}" \
  mongod --logpath /var/log/mongodb/mongod.log
 
echo "MongoDB avviato correttamente."
echo "Dati -> $DATA_DIR"
echo "Log  -> $LOGS_DIR"
