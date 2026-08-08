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

Il formato predefinito è `tar.gz`. Sono disponibili `gzip`, `xz`, `zstd` e
`lz4`:

```bash
docker dvol backup --compression gzip app
docker dvol backup --compression xz --level 6 app
docker dvol backup --compression zstd --level 10 app
docker dvol backup --compression lz4 --level 6 --threads 4 app
```

Tutti gli algoritmi usano un livello bilanciato quando `--level` è omesso o
vale `0`. Il default bilanciato è il livello `6` per `gzip`, `xz`, `zstd` e
`lz4`. Gli archivi prodotti sono compatibili con i rispettivi formati:
`.tar.gz`, `.tar.xz`, `.tar.zst` e `.tar.lz4`.

| Algoritmo | Livelli supportati | Default | Thread |
| --- | --- | --- | --- |
| `gzip` (pgzip) | 1-9 | 6 | default CPU/2 |
| `xz` | preset 0-9 | 6 | sempre 1: la libreria xz è single-threaded |
| `zstd` | 1-22 | 6 | default CPU/2 |
| `lz4` | 1-9 high-compression | 6 | default CPU/2 |

Il numero predefinito di thread è `max(1, numero_cpu/2)`. Per `xz`,
`--threads` può essere omesso oppure impostato a `1`; valori superiori vengono
rifiutati perché non possono essere applicati dall’encoder. `gzip` usa
`pgzip`, che produce gzip standard decomprimibile con gli strumenti normali.

Opzioni di `backup`:

```text
--compression <gzip|xz|zstd|lz4> formato, default gzip
--level <n>                      livello, 0 usa il default bilanciato (6)
--threads <n>                    thread, 0 = max(1, numero_cpu/2); xz usa 1
--archive-name VALUE          flag ripetibile; con un volume: nome base;
                              con più volumi: VOLUME::NOME
--name VALUE                  alias di --archive-name
--same-destination            salva gli archivi direttamente in <destination>;
                              default: false, mantiene <destination>/<volume>/
--timestamp                   aggiunge _ddmmyyyy_HHMMSS; default: false
--timestamp-utc               usa UTC per --timestamp; default: timezone locale
--overwrite                   sostituisce un archivio locale esistente;
                              default: false, collisioni rifiutate
--bundle                      produce un unico bundle multi-volume;
                              necessario con più volumi in modalità --stdout
--stdout                     scrive soltanto l’archivio binario su stdout;
                              diagnostica su stderr; default: false
```

## Logging e avanzamento

Il livello si configura globalmente con `--log-level`:

```text
--log-level TRACE|DEBUG|INFO|WARN|ERROR    default: INFO
```

I livelli sono cumulativi: `INFO` mostra avvio, completamento, errori e
avanzamento; `DEBUG` aggiunge staging, validazioni e dettagli operativi;
`TRACE` aggiunge le singole entry archiviate/estratte; `WARN` mostra solo
avvisi ed errori; `ERROR` mostra esclusivamente gli errori.

Esempi:

```bash
docker dvol --log-level DEBUG backup --compression zstd mongo_data
docker dvol --log-level TRACE restore --stdin mongo_data < backup.tar.zst
```

Durante backup e restore vengono riportati separatamente i byte di dati
elaborati e quelli dello stream compresso, con unità dinamiche `B`, `KB`, `MB`,
`GB` e `TB`, per esempio:

```text
2026-08-08T05:40:54+02:00 [INFO] backup stream progress data="12.4 MB" stream="3.1 MB"
```

Tutti i messaggi, inclusi progress ed errori, sono scritti su `stderr`.
In particolare `--stdout` rimane esclusivamente l’archivio binario e non viene
mai contaminato dal logging.

Le stesse opzioni `--compression`, `--level` e `--threads` sono disponibili
anche per `docker dvol compose-backup`.

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

### Backup in streaming verso rclone

`--stdout` non crea il file archivio e non usa un file temporaneo per conservarne
lo stream. È pensato anche per volumi più grandi dello spazio libero locale:

```bash
docker dvol backup --stdout --compression zstd mongo_data \
  | rclone rcat myremote:/dumps/mongo/backup-mongo-dev.tar.zst
```

`rclone rcat` richiede sempre il percorso completo dell’oggetto remoto. Non
passare soltanto una directory e non aspettarsi che il nome venga generato da
dvol. Se l’oggetto esiste, rclone lo sovrascrive secondo la propria semantica;
per evitare collisioni usare un nome univoco o una directory `.partial` gestita
dallo script chiamante.

In modalità stream `--archive-name`, `--timestamp`, `--timestamp-utc`,
`--same-destination`, `--overwrite` e `--destination` vengono rifiutati: il
nome remoto e il timestamp del file sono responsabilità del comando che
consuma stdout. Esempio:

```bash
stamp="$(date -u +%d%m%Y_%H%M%S)"
set -o pipefail
docker dvol backup --stdout --compression zstd mongo_data \
  | rclone rcat "myremote:/dumps/mongo/mongodb-vol_$stamp.tar.zst"
```

Tutti i messaggi di stato ed errore sono su stderr, quindi stdout può essere
passato senza contaminazioni a rclone, SSH, cifratura o altro programma.

Per più volumi usare esplicitamente `--bundle`: viene prodotto un solo stream
con `manifest.json` e una directory per volume. Non concatenare archivi singoli.

```bash
docker dvol backup --stdout --bundle --compression zstd \
  pg_data filestore_data \
  | rclone rcat myremote:/dumps/odoo/odoo-volumes.tar.zst
```

Con più volumi, omettere `--bundle` è un errore. Il bundle viene ripristinato
completo; non è previsto il restore selettivo nella modalità stdin.

`--stdout` non gestisce retry, retention, lista dei backup o atomicità del
backend remoto. Per trasferimenti critici, lo script può caricare un nome
temporaneo e promuoverlo dopo la verifica:

```bash
set -euo pipefail
id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
tmp="myremote:/dumps/mongo/.partial/mongo_$id.tar.zst"
final="myremote:/dumps/mongo/mongo_$id.tar.zst"
docker dvol backup --stdout --compression zstd mongo_data \
  | rclone rcat "$tmp"
rclone size "$tmp"
rclone moveto "$tmp" "$final"
```

Questa promozione dipende dal backend: `moveto` può essere rename oppure copia
più cancellazione. La modalità stream evita il file temporaneo locale del
backup, ma non può garantire da sola la durabilità remota.

Il timestamp viene calcolato una sola volta per l’intero comando. Con
`--same-destination` i nomi archivio devono essere univoci; un archivio già
esistente causa errore e non viene sovrascritto.

Il timestamp predefinito usa la timezone locale per compatibilità. Con
`--timestamp-utc` viene usato UTC. `--overwrite` abilita esplicitamente la
sostituzione atomica di un archivio locale esistente; senza il flag il default
è sicuro e ripetere lo stesso comando fallisce senza alterare il backup valido.

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

### Restore da stdin

Un archivio remoto può essere letto con `rclone cat`:

```bash
rclone cat myremote:/dumps/mongo/backup-mongo-dev.tar.zst \
  | docker dvol restore --stdin mongo_data
```

Il default `--stdin` non conserva l’archivio compresso su disco: lo legge in
stream, lo decomprime e lo estrae in una directory di staging privata. Il
volume live viene sostituito soltanto dopo EOF, validazione tar/compressione e
metadata. Lo staging dei dati ripristinati è necessario per mantenere
l’atomicità; non è una copia temporanea dell’archivio.

È disponibile anche la modalità esplicita con spool:

```bash
rclone cat myremote:/dumps/mongo/backup-mongo-dev.tar.zst \
  | docker dvol restore --stdin --spool mongo_data
```

`--spool` conserva prima tutto lo stream in un file temporaneo locale e poi
esegue il restore. Usa più spazio, ma permette di rileggere l’input e isolare
ulteriormente il trasporto dal restore. Il file viene rimosso anche in caso di
errore.

Per un bundle diretto:

```bash
rclone cat myremote:/dumps/odoo/odoo-volumes.tar.zst \
  | docker dvol restore --stdin --bundle pg_data filestore_data
```

Il bundle viene ripristinato interamente: i nomi indicati devono corrispondere
a tutti i volumi dichiarati nel manifest. Un input troncato o corrotto lascia
invariati tutti i volumi target.

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
compose-backup [-f <file>] [-p <project>] [--stdout] [--compression <formato>] [--level <n>] [--threads <n>]
compose-restore [-f <file>] [-p <project>] [--stdin] [--spool] [<archivio>]
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

`compose-backup --stdout` produce sempre un bundle unico, adatto a rclone:

```bash
docker compose down
docker dvol compose-backup --stdout -f compose.yaml -p demo \
  --compression zstd \
  | rclone rcat myremote:/dumps/demo/demo-volumes.tar.zst
```

Restore Compose da remoto, senza spool dell’archivio:

```bash
rclone cat myremote:/dumps/demo/demo-volumes.tar.zst \
  | docker dvol compose-restore --stdin -f compose.yaml -p demo
```

Per usare la seconda modalità di restore:

```bash
rclone cat myremote:/dumps/demo/demo-volumes.tar.zst \
  | docker dvol compose-restore --stdin --spool -f compose.yaml -p demo
```

`compose-restore` richiede il Compose file per determinare i target e il
progetto per risolvere i nomi. Il manifest deve corrispondere esattamente ai
volumi del progetto; un mismatch causa errore prima degli swap. Il restore è
completo, non selettivo. Fermare o quiescere i servizi prima del restore.

## Consistenza e interruzioni

Il backup è progettato come operazione strettamente read-only sul volume
sorgente. Prima di iniziare rifiuta una sorgente che sia un symlink e rifiuta
una destinazione locale collocata dentro il volume. La traversata usa
descriptor Linux `O_NOFOLLOW|O_NOATIME`, verifica l’identità degli inode prima
di leggere e preserva anche i timestamp dei symlink; se il filesystem non
consente una lettura senza aggiornare l’atime, il backup fallisce invece di
procedere in modo non sicuro.

Queste garanzie proteggono il volume dall’azione di dvol, anche in caso di
errore dello stream, errore di scrittura remoto o processo terminato durante
la creazione dell’archivio. Non costituiscono però una snapshot filesystem: se
un container scrive durante la lettura, l’archivio può contenere dati
provenienti da istanti diversi. Per una copia coerente fermare prima i
container:

```bash
docker compose stop
docker dvol compose-backup -f compose.yaml
```

Il progetto non usa snapshot filesystem automatiche.

In caso di crash durante il backup:

- il volume sorgente non viene modificato;
- un archivio temporaneo incompleto non viene pubblicato;
- gli archivi già pubblicati restano validi.

La destinazione dell’archivio deve essere esterna a tutti i volumi sorgente.
Questo comando viene rifiutato prima di creare file:

```bash
docker dvol backup --destination "$(docker volume inspect -f '{{.Mountpoint}}' app)/backup" app
```

In modalità `--stdout` non viene creato il file temporaneo dell’archivio: se il
consumer remoto interrompe la pipe, dvol segnala l’errore e non può controllare
se il backend abbia lasciato un oggetto parziale. Per questo il percorso remoto
deve essere verificato dal chiamante; `rclone rcat` non può ritentare da capo
una pipe già consumata.

In caso di errore durante `--stdin`, la decompressione e il trailer vengono
consumati e verificati prima del commit. Nella modalità senza spool non viene
conservato l’archivio compresso, ma viene comunque usato lo staging dei dati
necessario allo swap atomico. Un bundle viene pubblicato solo dopo la
validazione completa di tutti i volumi.

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
make acceptance-stream
make clean
```

Il target va eseguito dalla directory root del repository, non da
`test/acceptance/`. In alternativa, da una sottodirectory usare
`sudo make -C ../.. acceptance`.

`make acceptance` esegue gli scenari PostgreSQL e Odoo con tutte le
compressioni (`gzip`, `xz`, `zstd`, `lz4`) due volte: prima usando il binario
direttamente e poi usando realmente il comando installato `docker dvol`.
Prima dell’esecuzione copia il binario CLI in
`$HOME/.docker/cli-plugins/docker-dvol` dell’utente effettivo (quindi in
`/root/.docker/cli-plugins/` quando viene lanciato con `sudo`). Richiede un
Docker Engine operativo, permessi sui mountpoint dei volumi e può scaricare le
immagini di test. Lo script rimuove al termine container, volumi, reti Compose,
repository temporaneo e le immagini scaricate esclusivamente dalla suite.

`make acceptance-stream` esegue la matrice stdin/stdout attraverso `docker
dvol` e l’immagine Docker ufficiale `rclone/rclone:latest`, usando una directory
locale come remote filesystem. Verifica sul remote l’esistenza, la dimensione
e la rilettura di ogni oggetto; quindi esegue restore diretto e `--spool` per
backup singoli, bundle multi-volume e Compose, con tutte le compressioni. La
matrice include anche stream troncati e verifica che i volumi non cambino. Se
l’host non consente al processo utente di leggere i mountpoint Docker, lo
script esegue il plugin in un container `docker:cli` con il socket Docker e il
percorso dei volumi montati; in alternativa eseguire `sudo make acceptance`.

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

I test unitari coprono backup read-only con verifica di contenuto, ownership,
permessi, mtime, atime, symlink e hardlink; destinazioni non sicure, errori di
writer, stream troncati e processo terminato durante il publish; restore
atomico, rollback, compressione, bundle Compose, interruzioni e gestione di
mountpoint Docker. `make acceptance-stream` ripete inoltre le verifiche di
invariabilità sui volumi Docker reali durante backup singoli, bundle e Compose.

## Layout

```text
cmd/dvol-cli-plugin/   plugin del client Docker (`docker dvol`)
pkg/dvol/              archivio backup/restore e parsing Compose
test/                  smoke test
Makefile               build, test, lint e installazione
```
