# Use case to implements

Voglio poter usare il plufin direttamente da dentro docker

- docker volume backup <volume-name>
- docker volume backup <volume-name-1>, <volume-name-2>,... <volume-name-N>
- docker volume restore <volume-name>
- docker volume restore <volume-name-1>, <volume-name-2>,... <volume-name-N>

Ed anche in docker compose

- docker compose volumes backup
- docker compose volumes restore
- docker compose -f mycompose.yml volumes backup
- docker compose -f mycompose.yml volumes restore

Per il compose, deve essere prodotto un solo file di backup contenente tutti i volumi.