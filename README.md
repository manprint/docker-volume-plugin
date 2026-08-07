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

Il plugin viene installato in `~/.docker/cli-plugins/docker-dvol`. Per
aggiornarlo, ripetere `make build` e `make cli-plugin-install`.

Non è necessario installare o abilitare un Docker managed plugin. I volumi
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
    directory degli archivi;
    default: <root>/backups
```

Il repository contiene solo gli archivi; non contiene i dati operativi dei
volumi Docker. Esempio:

```bash
docker dvol --root "$HOME/docker-volume-backups" list
docker dvol --root "$HOME/docker-volume-backups" backup app
```

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
```

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
make clean
```

Il target va eseguito dalla directory root del repository, non da
`test/acceptance/`. In alternativa, da una sottodirectory usare
`sudo make -C ../.. acceptance`.

`make acceptance` esegue gli scenari PostgreSQL e Odoo con tutte le
compressioni (`gzip`, `xz`, `zstd`). Richiede un Docker Engine operativo,
permessi sui mountpoint dei volumi e può scaricare le immagini di test. Lo
script rimuove al termine container, volumi, reti Compose, repository
temporaneo e le immagini scaricate esclusivamente dalla suite.

I test unitari coprono backup read-only, restore atomico, rollback,
compressione, bundle Compose, interruzioni e gestione di mountpoint Docker.

## Layout

```text
cmd/dvol-cli-plugin/   plugin del client Docker (`docker dvol`)
pkg/dvol/              archivio backup/restore e parsing Compose
test/                  smoke test
Makefile               build, test, lint e installazione
```
