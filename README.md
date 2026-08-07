# dvol — Docker volume plugin con backup e restore

`dvol` è un volume driver per Docker che salva i volumi in una directory
locale dell’host e aggiunge backup e restore in formato tar compresso.

Il progetto comprende:

- il plugin Docker `dvol`, che crea e monta i volumi;
- `dvol-ctl`, CLI host-side per gestire volumi e archivi;
- il plugin CLI `docker dvol`, che integra i comandi nel client Docker;
- backup Compose multi-volume con un unico archivio e un `manifest.json`.

## Requisiti

- host Linux con Docker Engine e supporto ai managed plugin;
- Go `1.26.1` o superiore per compilare il progetto;
- Docker e GNU Make per costruire e installare il plugin;
- privilegi sufficienti per installare/abilitare un Docker plugin e leggere la
  directory di backing store.

Il backing store predefinito è `/var/lib/dvol`. Il plugin Docker monta questa
directory nel proprio filesystem interno come `/data`.

> `make plugin-install` usa `--grant-all-permissions`, perché il driver deve
> poter gestire mount e volumi. Valutare questa scelta secondo il modello di
> sicurezza dell’host.

## Installazione dal sorgente

```bash
git clone <repository-url> docker-volume-plugin
cd docker-volume-plugin

# Compila dvol-plugin, dvol-ctl e il plugin docker dvol.
make build
```

I binari vengono creati in `bin/`:

```
bin/dvol-plugin
bin/dvol-ctl
bin/dvol-cli-plugin
bin/docker-dvol-linux
```

### Installare il volume plugin Docker

Il comando seguente costruisce l’immagine rootfs, crea il plugin e lo installa:

```bash
make plugin-install
```

Verificare lo stato:

```bash
docker plugin ls
docker plugin inspect dvol
```

Se il plugin è installato ma disabilitato:

```bash
make plugin-enable
# oppure:
docker plugin enable dvol
```

Creare e usare un volume:

```bash
docker volume create --driver dvol --name app

docker run --rm \
  --mount source=app,target=/data \
  alpine sh -c 'printf "hello\n" >/data/message.txt'

docker run --rm \
  --mount source=app,target=/data \
  alpine cat /data/message.txt
```

Il volume resta sull’host in `/var/lib/dvol/volumes/app/`.

## Volumi Docker esistenti e driver `local`

`docker dvol` opera sui volumi Docker, non su un percorso alternativo scelto
dall’utente. Prima di ogni operazione interroga Docker e usa il mountpoint
reale del volume. Questo significa che può gestire anche volumi creati prima
dell’installazione del progetto o creati con il driver predefinito `local`:

```bash
docker volume create existing-data
docker dvol list
docker dvol backup existing-data
docker dvol backups existing-data
docker dvol restore existing-data
```

Per un volume creato con `--driver dvol`, il comando usa il backing store di
dvol. Per un volume `local` (o per un altro driver con un mountpoint
accessibile) usa direttamente quel mountpoint. Non è quindi necessario
importare o ricopiare i dati in un secondo volume.

Controllare il driver e il mountpoint di un volume:

```bash
docker volume inspect existing-data \
  --format 'name={{.Name}} driver={{.Driver}} mountpoint={{.Mountpoint}}'
```

Se il volume è già gestito da dvol, usare lo stesso `--root` con cui è stato
avviato il plugin. Se il volume è `local`, `docker dvol` lo usa direttamente:
non modifica il driver e non sposta i dati.

### Creare un volume nuovo durante il restore

Se si ripristina un archivio su un nome che non esiste ancora, `docker dvol`
crea automaticamente un normale volume Docker `local` con quel nome e poi
estrae i dati nel suo mountpoint:

```bash
docker dvol restore new-data /mnt/backup-dvol/existing-data/existing-data-20260807-120000.tar.gz
docker volume inspect new-data
```

Il restore non converte il volume in `dvol`: il driver resta quello dichiarato
da Docker. Per creare esplicitamente un volume dvol si può comunque usare:

```bash
docker volume create --driver dvol --name app
```

> Un managed plugin Docker non può intercettare né cambiare il comportamento del
> driver `local`. Per questo `docker volume create --name app` continua a creare
> un volume `local`; la gestione trasparente dei volumi di qualunque driver è
> fornita da `docker dvol backup`, `restore`, `list` e dai comandi Compose.

### Copia opzionale verso un volume dvol

La copia verso un volume dvol serve solo se si vuole cambiare driver, non per
eseguire il backup. Fermare prima i container che scrivono sul volume:

```bash
docker run --rm --user 0 \
  --mount source=existing-data,target=/from,readonly \
  --mount source=app,target=/to \
  alpine sh -c 'cd /from && tar cpf - . | tar xpf - -C /to'
```

Questa è una migrazione facoltativa. Verificare il contenuto prima di
riconfigurare i servizi e non eliminare il volume originale finché la copia non
è stata controllata.

### Accesso ai mountpoint Docker

Per i volumi non gestiti dal backing store dvol, il plugin CLI legge il percorso
restituito da `docker volume inspect`. Di conseguenza:

- il comando deve essere eseguito sull’host che espone quel mountpoint;
- l’utente deve poter leggere/scrivere la directory, oppure il comando va
  eseguito con privilegi adeguati;
- un Docker daemon remoto o Docker Desktop può restituire un mountpoint che non
  esiste nel filesystem locale: in quel caso il volume va gestito dal client
  presente sull’host del daemon.

Per un backup coerente fermare i container che scrivono sul volume:

```bash
docker compose stop
# oppure fermare esplicitamente i container interessati
```

La stessa gestione diretta vale per i volumi Compose esistenti:

```yaml
volumes:
  data:
    name: existing-data
    external: true
```

### Installare `docker dvol`

Il plugin CLI è separato dal volume plugin Docker. Installarlo con:

```bash
make cli-plugin-install
docker dvol --help
```

Il file viene copiato in `~/.docker/cli-plugins/`. Se si usa un backing store
diverso da `/var/lib/dvol`, specificarlo con `--root` a ogni comando oppure
usare `dvol-ctl` con lo stesso percorso.

## Come sono organizzati i dati

Con root `/var/lib/dvol`, il layout è:

```
/var/lib/dvol/
├── volumes/
│   └── <volume>/                 dati del volume
├── meta/
│   └── <volume>.json             metadati del volume
└── backups/
    ├── <volume>/
    │   └── <volume>-<timestamp>.tar.gz
    └── compose/
        └── <project>-<timestamp>.tar.gz
```

Gli archivi possono avere estensione `.tar.gz`, `.tar.xz` oppure
`.tar.zst`. Il formato predefinito è gzip.

## Workflow rapido

```bash
# Elenca i volumi dvol.
docker dvol list

# Crea un backup del volume app.
docker dvol backup app

# Elenca gli archivi disponibili.
docker dvol backups app

# Ripristina l’ultimo backup disponibile.
docker dvol restore app
```

Prima di un restore è consigliato fermare i container che usano il volume.
Il restore sostituisce il contenuto del volume; non è un merge dei file.

## `dvol-ctl`: CLI host-side

`dvol-ctl` lavora direttamente sul backing store e non richiede che il comando
passi dal Docker daemon.

Se non è stato installato nel `PATH`, usare `bin/dvol-ctl`.

### Opzioni globali

Le opzioni globali vanno messe prima del comando:

```
--root <dir>       backing store (default: /var/lib/dvol)
--backup-dir <dir> directory degli archivi (default: <root>/backups)
```

Esempio con percorsi personalizzati:

```bash
bin/dvol-ctl \
  --root /srv/dvol \
  --backup-dir /mnt/backup-dvol \
  list
```

### Gestione dei volumi

```bash
# Lista volumi; alias: ls.
bin/dvol-ctl list

# Crea uno o più volumi.
bin/dvol-ctl create app db cache

# Elimina dati e metadati di uno o più volumi; alias: remove.
bin/dvol-ctl rm cache
```

`rm` è distruttivo: verificare di avere un backup prima di usarlo.

### Backup

```bash
# Backup gzip, livello predefinito.
bin/dvol-ctl backup app

# Backup di più volumi, un archivio per volume.
bin/dvol-ctl backup app db

# Compressione e livello espliciti.
bin/dvol-ctl backup \
  --compression zstd \
  --level 6 \
  app db

# Sintassi alternativa con '='.
bin/dvol-ctl backup --compression=xz --level=6 app
```

Opzioni del comando `backup`:

| Opzione | Valori | Default |
| --- | --- | --- |
| `--compression` | `gzip`, `xz`, `zstd` | `gzip` |
| `--level` | gzip `0–9`, xz `0–9`, zstd `0–22` | `0` |

Il livello `0` seleziona il default del compressore. Ogni volume produce un
archivio separato in `<backup-dir>/<volume>/`.

### Elenco e restore

```bash
# Elenca gli archivi del volume, dal più vecchio al più recente.
bin/dvol-ctl backups app

# Ripristina un file indicato con percorso assoluto o relativo.
bin/dvol-ctl restore app /mnt/backup-dvol/app/app-20260807-120000.tar.gz

# Seleziona un file per nome nella directory dei backup del volume.
bin/dvol-ctl restore app app-20260807-120000.tar.gz

# È possibile usare un glob, purché corrisponda a un solo archivio.
bin/dvol-ctl restore app 'app-20260807-*.tar.gz'
```

Se il volume di destinazione non esiste, `restore` lo crea automaticamente.

## `docker dvol`: plugin del client Docker

`docker dvol` usa lo stesso backing store di `dvol-ctl`, offre una sintassi
integrata nel comando `docker` e, per ogni volume, interroga anche il Docker
daemon. È il punto di ingresso consigliato quando si vogliono gestire insieme
volumi dvol e volumi Docker già esistenti.

### Opzioni globali

```
--root <dir>       backing store (default: /var/lib/dvol)
--backup-dir <dir> directory degli archivi (default: <root>/backups)
```

Esempio con un backing store non predefinito:

```bash
docker dvol --root /srv/dvol --backup-dir /mnt/backup-dvol list
```

### Comandi disponibili

```
docker dvol list
docker dvol backups <volume>

docker dvol backup [--compression gzip|xz|zstd] [--level N] <volume>...
docker dvol restore <volume>... [backup]
```

`list` mostra i volumi restituiti da Docker, inclusi quelli `local`; i volumi
presenti solo nel backing store vengono mostrati come `dvol`.

I nomi dei volumi possono essere separati da spazi oppure da virgole:

```bash
docker dvol backup app db cache
docker dvol backup app,db,cache
```

Esempi di backup compressi:

```bash
docker dvol backup --compression gzip --level 9 app
docker dvol backup --compression xz --level 6 app
docker dvol backup --compression zstd --level 10 app
```

Esempi di restore:

```bash
# Ripristina l’ultimo backup di app.
docker dvol restore app

# Ripristina un archivio specifico di app.
docker dvol restore app app-20260807-120000.tar.gz

# Ripristina l’ultimo backup di più volumi.
docker dvol restore app db
```

Quando si specifica un archivio esplicito, il comando deve riferirsi a un solo
volume:

```bash
docker dvol restore app /mnt/backup-dvol/app/app-20260807-120000.tar.zst
```

## Backup e restore Compose

Il comando Compose crea un unico archivio per il progetto, con un
`manifest.json` e una directory di primo livello per ogni volume. Risolve i
nomi tramite Docker, quindi include anche volumi `local` ed esistenti; per i
volumi dvol usa invece direttamente il backing store.

```bash
# Cerca automaticamente compose.yaml, compose.yml,
# docker-compose.yaml o docker-compose.yml nella directory corrente.
docker dvol compose-backup

# Specifica il file Compose.
docker dvol compose-backup -f compose.yaml

# Sovrascrive il nome del progetto.
docker dvol compose-backup -f compose.yaml -p demo

# Backup Compose con zstd.
docker dvol compose-backup \
  -f compose.yaml \
  -p demo \
  --compression zstd \
  --level 6
```

Gli archivi vengono salvati in:

```
<backup-dir>/compose/<project>-<timestamp>.tar.gz
```

Restore dell’ultimo bundle del progetto:

```bash
docker dvol compose-restore -f compose.yaml -p demo
```

Restore di un bundle specifico:

```bash
docker dvol compose-restore \
  -f compose.yaml \
  -p demo \
  /mnt/backup-dvol/compose/demo-20260807-120000.tar.zst
```

### Nomi dei volumi Compose

Per una dichiarazione standard il nome reale è `<project>_<volume>`:

```yaml
name: demo

services:
  app:
    image: alpine
    volumes:
      - data:/data

volumes:
  data: {}
```

In questo esempio il volume Docker atteso è `demo_data`, indipendentemente dal
driver usato per crearlo.

È possibile usare un nome esplicito oppure un volume esterno:

```yaml
volumes:
  data:
    name: dati-condivisi
  cache:
    external: true
```

In questo caso i nomi attesi sono rispettivamente `dati-condivisi` e
`cache`. Il backup Compose include i volumi dichiarati che esistono già in
Docker (e i volumi dvol presenti nel backing store). Il restore può crearli se
mancanti.

## Consistenza, sicurezza e limiti

- Il backup non modifica il volume sorgente: legge dati, metadati, symlink e
  hardlink senza alterare la directory del volume.
- Gli archivi vengono costruiti in un file temporaneo, sincronizzati e
  pubblicati con rename atomico. Un errore durante il backup non rende visibile
  un archivio parziale.
- Il restore estrae prima in staging, valida i percorsi e poi sostituisce il
  volume. Sono rifiutati path assoluti, traversal e tentativi di uscita tramite
  symlink.
- Un restore sostituisce il volume. Fermare i container che lo stanno usando
  prima di eseguirlo.
- Il backup è read-only, ma non è uno snapshot filesystem: se un’applicazione
  scrive mentre `tar` legge, l’archivio può rappresentare stati diversi del
  volume. Per una fotografia coerente fermare/quiescere i writer.
- Il formato conserva ownership, permessi, timestamp, symlink e hardlink. Per
  ripristinare fedelmente ownership e permessi servono privilegi adeguati.
- I nomi dei volumi devono rispettare la regola Docker:
  `[a-zA-Z0-9][a-zA-Z0-9_.-]*`.
- I metadati del volume sono separati dall’archivio dati. Un restore su un
  volume nuovo crea metadati standard, ma non ricostruisce eventuali opzioni
  Docker non presenti nell’archivio.

## Operazioni di sviluppo e manutenzione

```bash
# Compila tutti i binari.
make build

# Test unitari.
make test

# Static check con go vet.
make lint

# Smoke test end-to-end della CLI.
make e2e

# Costruisce solo il rootfs del plugin.
make plugin-build

# Crea il pacchetto installabile del plugin.
make plugin-create

# Disabilita e rimuove il plugin Docker.
make plugin-remove

# Sviluppo locale senza installare un Docker plugin.
make dev

# Sviluppo locale con root personalizzata.
make dev-root DEV_ROOT=/tmp/dvol-dev
```

`make dev` usa `./dev-dvol` come root e mantiene il processo in foreground.
`make clean` rimuove binari, rootfs, `dev-dvol` e gli artefatti di sviluppo.

## Risoluzione dei problemi

### `docker dvol` non viene trovato

Reinstallare il plugin CLI e verificare che Docker lo riconosca:

```bash
make cli-plugin-install
docker dvol --help
```

### Il plugin `dvol` è disabilitato

```bash
docker plugin ls
docker plugin enable dvol
```

### I comandi vedono volumi diversi

Assicurarsi che tutti usino lo stesso root:

```bash
docker dvol --root /srv/dvol list
bin/dvol-ctl --root /srv/dvol list
```

Il plugin Docker installato tramite il manifest usa invece `/var/lib/dvol`,
montato internamente come `/data`.

### Compose backup non trova volumi

Controllare il nome reale creato da Docker:

```bash
docker volume ls
docker dvol list
```

Verificare `name:`, `external:` e il project name (`name:` nel file Compose o
`-p`). Il backup Compose richiede che i volumi esistano già in Docker oppure
nel backing store dvol.

### Restore senza archivio

Elencare gli archivi e usare il nome completo o un percorso:

```bash
docker dvol backups app
docker dvol restore app <nome-archivio.tar.gz>
```

### Permessi negati

Il processo deve poter leggere il backing store e scrivere nella directory dei
backup. Per i volumi Docker non dvol deve inoltre poter leggere/scrivere il
mountpoint restituito da `docker volume inspect`. Sul percorso predefinito può
essere necessario usare `sudo`:

```bash
sudo bin/dvol-ctl --root /var/lib/dvol list
sudo bin/dvol-ctl --root /var/lib/dvol backup app
```

## Licenza

Consultare i file di licenza e gli avvisi del repository.
