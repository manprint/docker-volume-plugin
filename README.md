# Docker Volume Backup

`docker dvol` è un’estensione del client Docker per eseguire backup e restore
dei volumi Docker. Usa il mountpoint reale del volume: non servono volumi
speciali, driver dedicati o copie manuali dei dati.

```bash
docker volume create app
docker dvol backup app
docker dvol restore app
```

L’installazione non modifica i volumi esistenti. Sono quindi gestiti anche i
volumi creati prima dell’installazione.

## Requisiti

- Docker Engine e il comando `docker` sull’host che esegue il comando;
- Go 1.26.1 o superiore per compilare il progetto;
- permessi di lettura/scrittura sul mountpoint Docker;
- container fermi o quiescenti durante il backup, se serve una fotografia coerente.

Il comando deve essere eseguito sul nodo del Docker daemon. Con un daemon
remoto o Docker Desktop, il mountpoint può trovarsi su un altro sistema.

## Installazione

```bash
git clone <repository-url> docker-volume-backup
cd docker-volume-backup
make build
make cli-plugin-install
docker dvol --help
```

Il plugin viene installato in `~/.docker/cli-plugins/docker-dvol`. Questo è un
Docker CLI plugin, quindi il nome del file è significativo; non devono esserci
copie con suffissi come `docker-dvol-linux`. Per aggiornarlo, ripetere `make
build` e `make cli-plugin-install`.

Non è necessario installare o abilitare un Docker Engine managed plugin. I volumi
restano volumi Docker normali, creati per esempio con:

```bash
docker volume create app
```

## Volumi esistenti e nuovi

`docker dvol` interroga Docker e usa il mountpoint restituito da
`docker volume inspect`:

```bash
docker volume ls
docker dvol list
docker dvol backup existing-data
```

Se il volume di destinazione non esiste durante un restore, viene creato
automaticamente con `docker volume create --name <nome>` e il backup viene
ripristinato nel suo mountpoint. Il volume rimane un volume Docker `local`.

```bash
docker volume inspect app \
  --format 'name={{.Name}} driver={{.Driver}} mountpoint={{.Mountpoint}}'
```

## Opzioni globali

```text
--root <dir>
    repository degli archivi;
    default: ~/.local/share/docker-volume-backup

--backup-dir <dir>
    directory radice degli archivi; alias storico di --destination;
    default: <root>/backups

--destination <dir>
	 directory radice degli archivi; gli archivi dei singoli volumi sono
	 organizzati in <dir>/<volume>/;
	 default: <root>/backups
```

Il repository contiene solo gli archivi; non contiene i dati operativi dei
volumi Docker. Esempio:

```bash
docker dvol --root "$HOME/docker-volume-backups" list
docker dvol --root "$HOME/docker-volume-backups" backup app
docker dvol --destination /home/ubuntu/dump-mongo-vol backup mongo_data
```

Con l’ultimo comando l’archivio viene scritto in
`/home/ubuntu/dump-mongo-vol/mongo_data/`. `--root` configura l’intero
repository; `--destination` (o `--backup-dir`) configura direttamente la
directory radice in cui vengono scritti gli archivi.

## Backup e restore di singoli volumi

Elenco dei volumi e degli archivi:

```bash
docker dvol list
docker dvol ls
docker dvol backups app
```

Backup di uno o più volumi:

```bash
docker dvol backup app
docker dvol backup app db cache
docker dvol backup app,db,cache
```

Il formato predefinito è `tar.gz`. Sono disponibili `gzip`, `xz` e `zstd`:

```bash
docker dvol backup --compression gzip app
docker dvol backup --compression xz --level 6 app
docker dvol backup --compression zstd --level 10 app
```

Opzioni di `backup`:

```text
--compression <gzip|xz|zstd>  formato, default gzip
--level <n>                   livello, 0 usa il default del formato
--archive-name VALUE          flag ripetibile; con un volume: nome base;
                              con più volumi: VOLUME::NOME
--name VALUE                  alias di --archive-name
--same-destination            salva gli archivi direttamente in <destination>;
                              default: false, mantiene <destination>/<volume>/
--timestamp                   aggiunge _ddmmyyyy_HHMMSS; default: false
```

Per esempio, per ottenere un backup MongoDB nella directory richiesta con
nome leggibile e timestamp:

```bash
docker dvol backup \
  --destination /home/ubuntu/dump-mongo-vol \
  --compression zstd \
  --archive-name mongodb-vol \
  --timestamp \
  mongo_data
```

Il risultato sarà simile a:

```text
/home/ubuntu/dump-mongo-vol/mongo_data/mongodb-vol_07082026_223010.tar.zst
```

Il formato `zstd` usa l’estensione `.tar.zst`; `.tar.zstd` indica lo stesso
formato ma non è il suffisso generato dal programma. Se non si usa
`--archive-name`, il nome base è quello del volume. Se non si usa
`--timestamp`, resta il nome predefinito con timestamp ad alta precisione.
Per più volumi usare un mapping ripetibile. Tutti i volumi selezionati devono
avere un mapping, i mapping duplicati vengono rifiutati e i nomi devono essere
nomi file, non percorsi. I flag devono essere indicati prima dei volumi:

```bash
docker dvol backup \
  --destination /home/ubuntu/stack_odoo \
  --compression zstd \
  --archive-name pg_data::postgres-odoo \
  --archive-name filestore_data::filestore-odoo \
  --same-destination \
  --timestamp \
  pg_data filestore_data
```

Con `--same-destination` il risultato è simile a:

```text
/home/ubuntu/stack_odoo/postgres-odoo_07082026_223010.tar.zst
/home/ubuntu/stack_odoo/filestore-odoo_07082026_223010.tar.zst
```

Senza `--same-destination` gli stessi archivi vengono invece scritti in:

```text
/home/ubuntu/stack_odoo/pg_data/postgres-odoo_07082026_223010.tar.zst
/home/ubuntu/stack_odoo/filestore_data/filestore-odoo_07082026_223010.tar.zst
```

Il timestamp viene calcolato una sola volta per l’intero comando. Con
`--same-destination` i nomi archivio devono essere univoci; un archivio già
esistente causa errore e non viene sovrascritto.

La modalità flat mantiene la trasparenza dei comandi esistenti tramite il file
di metadati persistente `.dvol-index.json`, aggiornato atomicamente. Perciò
continuano a funzionare anche:

```bash
docker dvol --destination /home/ubuntu/stack_odoo backups pg_data
docker dvol --destination /home/ubuntu/stack_odoo restore pg_data \
  postgres-odoo_07082026_223010.tar.zst
docker dvol --destination /home/ubuntu/stack_odoo restore pg_data
```

Il lock temporaneo dell’indice è `.dvol-index.lock`. Gli archivi flat già
pubblicati vengono recuperati anche se il processo si interrompe durante
l’aggiornamento dei metadati.

I permessi predefiniti restano invariati: file archivio e metadati `0644`,
directory create `0755`; owner e gruppo sono quelli dell’utente effettivo che
esegue il comando (`root:root` se eseguito con `sudo`).

Ripristinare l’ultimo archivio:

```bash
docker dvol restore app
docker dvol restore app db
```

Ripristinare un archivio preciso o un glob relativo a `backups/app/`:

```bash
docker dvol restore app app-20260807-120000.tar.gz
docker dvol restore app '*.tar.gz'
```

Il restore estrae prima in uno staging sullo stesso filesystem, valida
l’archivio e poi sostituisce il contenuto con uno swap atomico. Un archivio
invalido lascia intatto il volume esistente.

## Docker Compose

Dato un normale `compose.yaml`:

```yaml
name: demo

services:
  app:
    image: alpine
    command: ["sh", "-c", "sleep 3600"]
    volumes:
      - data:/var/lib/app
      - cache:/var/cache/app

volumes:
  data:
  cache:
```

Fermare i container e creare un bundle unico:

```bash
docker compose down
docker dvol compose-backup -f compose.yaml
```

Comandi:

```text
compose-backup [-f <file>] [-p <project>] [--compression <formato>] [--level <n>]
compose-restore [-f <file>] [-p <project>] [<archivio>]
```

Il bundle viene scritto in `backups/compose/`, contiene `manifest.json` e una
directory per ogni volume. Durante il restore i volumi mancanti vengono creati
automaticamente con Docker:

```bash
docker dvol compose-restore -f compose.yaml
docker dvol compose-restore -f compose.yaml \
  ~/.local/share/docker-volume-backup/backups/compose/demo-20260807-120000.tar.gz
```

Tutti gli staging vengono preparati prima degli swap; ogni volume viene
sostituito atomicamente e indipendentemente dagli altri.

## Consistenza e interruzioni

Il backup legge il volume senza modificarlo. La lettura, però, non è una
snapshot filesystem: se un container scrive durante `tar`, l’archivio può
contenere dati provenienti da istanti diversi. Per una copia coerente fermare
prima i container:

```bash
docker compose stop
docker dvol compose-backup -f compose.yaml
```

Il progetto non usa snapshot filesystem automatiche.

In caso di crash durante il backup:

- il volume sorgente non viene modificato;
- un archivio temporaneo incompleto non viene pubblicato;
- gli archivi già pubblicati restano validi.

In caso di crash durante il restore, staging e rollback usano percorsi privati
collision-free; le sostituzioni interrotte vengono recuperate all’avvio
successivo quando possibile.

## Permessi

Su Linux, i mountpoint dei volumi Docker sono spesso accessibili solo a root.
Se compare `permission denied`, usare un account autorizzato e mantenere il
repository degli archivi accessibile allo stesso account:

```bash
sudo docker dvol --root /srv/docker-volume-backups backup app
```

## Sviluppo e test

```bash
cd /percorso/del/repository/docker-volume-plugin
make build
make test
make lint
make e2e
sudo make acceptance
sudo make acceptance-mongo
make clean
```

Il target va eseguito dalla directory root del repository, non da
`test/acceptance/`. In alternativa, da una sottodirectory usare
`sudo make -C ../.. acceptance`.

`make acceptance` esegue gli scenari PostgreSQL e Odoo con tutte le
compressioni (`gzip`, `xz`, `zstd`) due volte: prima usando il binario
direttamente e poi usando realmente il comando installato `docker dvol`.
Prima dell’esecuzione copia il binario CLI in
`$HOME/.docker/cli-plugins/docker-dvol` dell’utente effettivo (quindi in
`/root/.docker/cli-plugins/` quando viene lanciato con `sudo`). Richiede un
Docker Engine operativo, permessi sui mountpoint dei volumi e può scaricare le
immagini di test. Lo script rimuove al termine container, volumi, reti Compose,
repository temporaneo e le immagini scaricate esclusivamente dalla suite.

Per eseguire uno script singolarmente, `DVOL_MODE=direct` è il default;
`DVOL_MODE=docker` forza il passaggio attraverso il plugin installato:

```bash
sudo DVOL_MODE=docker ./test/acceptance_e2e.sh
sudo DVOL_MODE=docker ./test/acceptance_mongo_e2e.sh
```

`make acceptance-mongo` esegue la stessa matrice MongoDB in entrambe le
modalità. La matrice è basata sui file
`test/acceptance/mongo-compose.yaml` e
`test/acceptance/script-start.mongo.sh`; il dettaglio è in
`test/acceptance/mongo_test_matrix.md`.

I test unitari coprono backup read-only, restore atomico, rollback,
compressione, bundle Compose, interruzioni e gestione di mountpoint Docker.

## Layout

```text
cmd/dvol-cli-plugin/   plugin del client Docker (`docker dvol`)
pkg/dvol/              archivio backup/restore e parsing Compose
test/                  smoke test
Makefile               build, test, lint e installazione
```
