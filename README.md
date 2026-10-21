# Docker Volume Backup

`docker dvol` è un’estensione del client Docker per eseguire backup e restore
dei volumi Docker e proteggerli dalla rimozione accidentale. Usa il mountpoint
reale del volume, o il `device` dei bind-backed supportati: non installa un
volume driver e non copia i dati fuori da Docker durante il normale
funzionamento.

```bash
docker volume create app
docker dvol backup app
docker dvol restore app
```

L’installazione non modifica i volumi esistenti. Sono quindi gestiti anche i
volumi creati prima dell’installazione.

## Requisiti

- Linux, Docker Engine locale e Docker CLI 19.03 o superiore sull’host che
  esegue il comando;
- Go 1.27.3 o superiore per compilare il progetto (la versione è fissata dalla
  direttiva `go` in `go.mod` e usata anche dalla CI);
- permessi di lettura/scrittura sul mountpoint Docker;
- volumi con driver `local` normali oppure bind-backed con opzioni
  `type=none`, `device=<percorso assoluto>`, `o=bind` (è ammesso anche `rw`);
- container fermi o quiescenti durante il backup, se serve una fotografia coerente.

Il comando deve essere eseguito sul nodo del Docker daemon. Con un daemon
remoto o Docker Desktop, il mountpoint si trova altrove: il plugin può essere
scoperto dal client, ma backup e restore vengono rifiutati o non sono
supportati. I volumi bind-backed locali sono risolti al loro `device` host;
volumi NFS/CIFS creati tramite opzioni del driver `local` e driver di terze
parti sono invece rifiutati.

### Output del client Docker

dvol interroga il daemon tramite il client `docker` e legge soltanto il suo
standard output. Gli avvisi che il client scrive su standard error (per esempio
`WARNING: API version negotiation failed`) non interferiscono quindi con la
decodifica: restano diagnostica e vengono riportati solo quando il comando
Docker fallisce davvero.

### Compatibilità Docker CLI

L’installabilità come `docker dvol` dipende dalla versione del **client**. Le
operazioni sui volumi dipendono invece dal daemon locale Linux e dai requisiti
appena elencati. Docker ha introdotto il framework dei CLI plugin nella serie
[19.03](https://docs.docker.com/engine/release-notes/19.03/); il contratto
metadata `0.1.0` usato da dvol è presente nel
[sorgente docker/cli 19.03.15](https://github.com/docker/cli/blob/v19.03.15/cli-plugins/manager/plugin.go).

| Docker CLI | Installabile come `docker dvol` | Stato |
| --- | --- | --- |
| 29.x | Sì | Discovery, help, E2E, streaming e acceptance applicativa/MongoDB verificati con CLI ed Engine 29.7.2. |
| 28.x | Sì | Compatibilità tecnica del contratto plugin; non certificato in QA. |
| 27.x | Sì | Compatibilità tecnica; non certificato in QA. |
| 26.x | Sì | Compatibilità tecnica; non certificato in QA. |
| 25.x | Sì | Compatibilità tecnica; non certificato in QA. |
| 24.x | Sì | Compatibilità tecnica; non certificato in QA. |
| 23.x | Sì | Compatibilità tecnica; non certificato in QA. |
| 20.10.x | Sì | Compatibilità tecnica storica; non certificato in QA. |
| 19.03.x | **Sì, minimo tecnico** | Discovery e help verificati a runtime con `docker:19.03.15`; data path su daemon storico non certificato. |
| 18.09.x | No | Verifica runtime negativa con `docker:18.09.9`: `dvol` non viene scoperto. |
| 18.06.x e precedenti | No | Non supportato. |

La tabella riguarda discovery e installazione, non certifica ogni combinazione
client/Engine/filesystem. Docker non ha pubblicato serie Engine stabili 21.x e
22.x; per questo la cronologia passa da 20.10 a 23.x. La matrice QA richiesta è
CLI 19.03.15 e corrente, Engine storico concordato e corrente, linux/amd64 e
linux/arm64, ext4 e XFS, con volumi `local` normali e bind-backed.

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
`docker volume inspect`; per un volume bind-backed supportato usa invece il
percorso assoluto dell'opzione `device`:

```bash
docker volume ls
docker dvol list
docker dvol backup existing-data
```

Se il volume di destinazione non esiste durante un restore, viene creato
automaticamente con `docker volume create --name <nome>` e il backup viene
ripristinato nel suo mountpoint. Il volume rimane un volume Docker `local`. Se
il restore fallisce, dvol rimuove il volume creato dall’operazione soltanto se è
ancora vuoto, non montato e con lo stesso mountpoint; dati comparsi nel frattempo
non vengono cancellati. Un’etichetta univoca
`com.github.fabio.dvol.restore-owner` distingue la creazione di questa sessione
da una creazione concorrente; resta sul volume dopo un restore riuscito come
traccia di provenienza.

Gli archivi non contengono la configurazione del driver Docker. Se un volume
bind-backed non esiste più, ricrearlo con le sue opzioni `type/device/o` prima
del restore; altrimenti dvol crea volutamente un normale volume `local`.

```bash
docker volume inspect app \
  --format 'name={{.Name}} driver={{.Driver}} mountpoint={{.Mountpoint}}'
```

## Protezione dalla cancellazione dei volumi

```bash
docker dvol lock app
docker dvol lock app                 # idempotente
docker dvol unlock app
```

Elenco dei lock attivi:

```bash
docker ps --filter label=com.github.fabio.dvol.volume-lock=1 \
  --format 'table {{.Names}}\t{{.Label "com.github.fabio.dvol.locked-volume"}}'
```

`lock` crea un piccolo container guardiano che referenzia il volume in sola
lettura. Il daemon considera quindi il volume “in use” e rifiuta
`docker volume rm`, `docker volume rm -f`, `docker volume prune` e la rimozione
del volume eseguita da `docker compose down -v`. Il guard è in esecuzione con
rete disabilitata, root filesystem read-only, nessuna capability, restart
policy `unless-stopped` ed etichette `com.github.fabio.dvol.*`. dvol riconosce
quelle etichette e non confonde il guard con un container applicativo durante
il controllo precedente al restore.

L'immagine predefinita è `busybox:1.36.1` e può essere sostituita, ad esempio
in un ambiente offline, con un'immagine locale che fornisca `tail`:

```bash
docker dvol lock --image registry.example/internal/busybox:1.36.1 app
```

`unlock` rimuove soltanto il guard correttamente etichettato e non elimina il
volume. Entrambi i comandi sono idempotenti. Con Docker Compose 5.x,
`compose down -v` stampa `Resource is still in use` e conserva il volume, ma
può comunque terminare con exit code 0: il CLI plugin non può modificare la
politica di uscita di Compose. Gli altri oggetti del progetto (container e
network) possono essere rimossi normalmente: il lock riguarda esclusivamente
il volume.

Questo lock protegge dagli errori operativi, non è un confine di
autorizzazione: chi amministra Docker può rimuovere direttamente il container
guardiano e poi cancellare il volume. Per un diniego non aggirabile dagli
utenti Docker servirebbe un authorization plugin configurato sul daemon.

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

--log-level <livello>
    TRACE, DEBUG, INFO, WARN o ERROR; default: INFO

--archive-mode <ottale>
    permessi degli archivi pubblicati e del file di indice;
    default: 0644. Usare 0600 per archivi leggibili solo dal proprietario

--allow-atime
    consente di leggere sorgenti che non possono essere aperte con O_NOATIME
    (file di un altro utente senza privilegi di root, alcuni mount NFS);
    default: il backup fallisce invece di aggiornare gli atime della sorgente

--best-effort-metadata
    durante il restore, registra un warning e continua se non può ricreare
    xattr, ACL, capability, FIFO o device; default sicuro: il restore fallisce
    e non pubblica alcun risultato incompleto
```

Le opzioni globali possono precedere o seguire il sottocomando. Dopo `--` non
vengono più interpretate, quindi un valore che assomiglia a un flag globale
resta al sottocomando.

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
                              default: false, mantiene <destination>/<volume>/;
                              incompatibile con --bundle (un bundle finisce
                              sempre in <destination>/compose) e viene rifiutato
                              invece di essere ignorato
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

Con più volumi, omettere `--bundle` è un errore. Vengono ripristinati i volumi
indicati che il bundle contiene; se uno di essi manca, il restore fallisce
(`--allow-missing` per avvisare e proseguire) e i volumi del bundle non
richiesti restano intatti.

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

Il lock temporaneo dell’indice è `.dvol-index.lock` e ogni backup in corso
mantiene una prenotazione `.dvol-reserve-<archivio>.lock`. Il lock dell’indice
non viene mai tenuto per tutta la scrittura dell’archivio, quindi `docker dvol
backups` risponde anche durante un backup lungo; la prenotazione impedisce a due
processi di scrivere lo stesso nome e permette di distinguere un backup vivo da
uno interrotto.

Tutti i file di controllo di dvol stanno **dentro** la directory degli archivi:
`.dvol-index.json`, `.dvol-index.lock`, `.dvol-reserve-<archivio>.lock`,
`.dvol-temp.lock` (coordina i file temporanei con la pulizia di quelli
abbandonati) e i temporanei `.dvol-backup-*` in corso di scrittura. dvol non
scrive nulla nella directory padre della destinazione: basta che la
destinazione sia scrivibile, anche se il suo padre non lo è (per esempio
`/mnt/nas` di root e `/mnt/nas/backups` dell’operatore). Nessuno di questi file
compare in `docker dvol backups`. Gli archivi flat già pubblicati vengono recuperati anche se il
processo si interrompe durante l’aggiornamento dei metadati, e una voce di
indice illeggibile viene scartata con un avviso senza bloccare né l’elenco né i
backup successivi.

I permessi predefiniti restano invariati: file archivio e metadati `0644`,
directory create `0755`; owner e gruppo sono quelli dell’utente effettivo che
esegue il comando (`root:root` se eseguito con `sudo`).

Ripristinare l’ultimo archivio:

```bash
docker dvol restore app
docker dvol restore app db
```

Con più nomi vengono ripristinati tutti i volumi indicati. Un volume che non
esiste più viene ricreato: è il caso tipico del disaster recovery.

Ripristinare un archivio preciso o un glob relativo a `backups/app/`:

```bash
docker dvol restore app app-20260807-120000.tar.gz
docker dvol restore app '*.tar.gz'
docker dvol restore --archive ~/dumps/app-20260807.tar.zst app
```

L’ultimo argomento viene interpretato come selettore di backup solo se ne ha la
forma: suffisso `.tar.gz|.tar.xz|.tar.zst|.tar.lz4`, presenza di `/` oppure di
caratteri glob (`*`, `?`, `[`). Altrimenti è un nome di volume. Con `--archive`
la scelta è esplicita e non dipende da euristiche.

Opzioni di `restore`:

```text
--archive <path|glob>   archivio da ripristinare (un solo volume)
--bundle                ripristina un bundle multi-volume
--stdin [--spool]       legge l’archivio da stdin
--allow-missing         con --bundle, avvisa invece di fallire se un volume
                        richiesto non è nel bundle
--force                 esegue il restore anche se container in esecuzione
                        montano il volume
```

Il restore estrae prima in uno staging sullo stesso filesystem, valida
l’archivio e poi sostituisce il contenuto con uno swap atomico. Un archivio
invalido lascia intatto il volume esistente.

### Volumi in uso

Sostituire la directory di un volume montato da un container in esecuzione non
è coerente: il container continua a usare la vecchia directory, che il restore
rimuove, e non vede mai i dati ripristinati. Per questo il restore viene
rifiutato:

```text
docker dvol: volume "app" is mounted by running container(s) app-1: a restore
replaces the directory they hold open, so they would keep writing to the old
data. Stop them first, or pass --force
```

Fermare i container, oppure usare `--force` accettandone le conseguenze.
Se Docker non riesce a determinare se il volume è in uso, il restore fallisce
allo stesso modo; `--force` è l’unico opt-in che permette di procedere senza
questa verifica. Il controllo viene eseguito sia prima dello staging sia subito
prima del commit, per ridurre la finestra in cui un container potrebbe montare
il volume durante un restore lungo. Non è un lock Docker: la procedura operativa
resta fermare o quiescere esplicitamente i workload.

### Archivi singoli e bundle non sono intercambiabili

Un bundle multi-volume passato al restore di un singolo volume viene rifiutato,
e viceversa:

```bash
docker dvol restore app backups/compose/demo-20260807.tar.gz
# docker dvol: archive is a multi-volume dvol bundle: restore it with --bundle
```

Senza questo controllo il volume verrebbe sostituito dal contenuto del bundle
(`manifest.json` più una directory per volume).

## Verifica degli archivi

`verify` legge l’archivio per intero — header tar, payload e trailer di
compressione — senza scrivere nulla. È il modo sicuro per sapere che un backup
è leggibile prima di averne bisogno:

```bash
docker dvol verify app                     # tutti gli archivi del volume
docker dvol verify app app-20260807.tar.gz # un archivio preciso
docker dvol verify --archive /dumps/x.tar.zst
docker dvol verify --json app
```

Output tipico:

```text
OK /home/u/.local/share/docker-volume-backup/backups/app/app-20260807-120000.tar.gz: volume, 128 entries, 12.4 MiB, 3 symlinks, 1 hardlinks
```

Un archivio danneggiato viene segnalato con `FAIL` e il comando esce con
stato 1.

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

I nomi indicati devono essere presenti nel manifest del bundle, altrimenti il
restore fallisce prima di toccare qualsiasi volume; con `--allow-missing`
l’assenza diventa un avviso. Un input troncato o corrotto lascia invariati tutti
i volumi target.

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
compose-backup [-f <file>] [-p|--project-name <project>] [--stdout] [--compression <formato>]
               [--level <n>] [--threads <n>] [--archive-name <nome>]
               [--timestamp] [--timestamp-utc] [--overwrite] [--skip-missing]
compose-restore [-f <file>] [-p|--project-name <project>] [--stdin] [--spool] [--strict]
                [--force] [<archivio>]
```

È accettato un solo `-f`: dvol non implementa le regole di merge di Compose.
Inoltre legge il YAML letterale e non espande variabili d'ambiente o profili.
Per più file o configurazioni interpolate, generare prima il risultato con
`docker compose ... config` e passare quel file a dvol.

Il nome progetto segue questa precedenza, dalla più forte: `-p`/
`--project-name`, `COMPOSE_PROJECT_NAME`, campo top-level `name:`, nome della
directory del Compose file. I primi tre valori devono già rispettare
`[a-z0-9][a-z0-9_-]*` e vengono rifiutati se non validi; soltanto il fallback
derivato dalla directory viene normalizzato in minuscolo.

Se un volume dichiarato nel compose file non esiste in Docker, `compose-backup`
si ferma: un bundle che contiene solo una parte del progetto è indistinguibile
da uno completo e il problema emergerebbe soltanto durante il restore.

```text
docker dvol: volume(s) demo_cache declared in compose.yaml do not exist in
Docker; create them or pass --skip-missing to back up only the existing ones
```

Con `--skip-missing` il backup prosegue e i volumi esclusi vengono elencati su
stderr.

Simmetricamente, `compose-restore` accetta per default un bundle che non
contiene tutti i volumi dichiarati: ripristina quelli presenti e avvisa sugli
altri, che restano intatti. Con `--strict` un bundle incompleto è un errore. In
entrambi i casi i volumi Docker mancanti vengono creati **solo dopo** che il
manifest è stato letto e accettato. Se una fase successiva fallisce, i volumi
creati dall’operazione vengono rimossi con le salvaguardie descritte sopra: un
restore rifiutato non lascia volumi vuoti.

Il bundle viene scritto in `backups/compose/`, contiene `manifest.json` e una
directory per ogni volume. Durante il restore i volumi mancanti vengono creati
automaticamente con Docker:

```bash
docker dvol compose-restore -f compose.yaml
docker dvol compose-restore -f compose.yaml \
  ~/.local/share/docker-volume-backup/backups/compose/demo-20260807-120000.tar.gz
```

Senza percorso esplicito viene scelto il bundle valido più recente del progetto;
un archivio singolo o un bundle con volumi estranei viene ignorato anche se il
nome inizia con quello del progetto. La
ricerca riconosce sia il nome predefinito `<progetto>-<timestamp>` sia quello
prodotto da `--timestamp` (`<progetto>_ddmmyyyy_HHMMSS`); un bundle rinominato
con `--archive-name` viene individuato leggendo il suo `manifest.json`.

Tutti gli staging vengono preparati e sincronizzati prima degli swap. Il bundle
ha un unico punto di commit journalizzato: se qualunque swap fallisce prima del
commit, tutti i volumi già sostituiti vengono riportati allo stato precedente;
dopo un crash, il recupero completa o annulla l’intero gruppo senza esporre un
bundle metà vecchio e metà nuovo.

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
progetto per risolvere i nomi. Il manifest viene confrontato con i volumi del
progetto prima di qualunque swap e prima di creare volumi Docker: per default un
volume dichiarato ma assente dal bundle produce un avviso e resta intatto, con
`--strict` è un errore. Fermare o quiescere i servizi prima del restore, oppure
usare `--force`.

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

### Backup a caldo

Un file che cambia dimensione mentre viene letto non interrompe il backup. La
dimensione registrata nell’header è quella vista all’apertura del file: se il
file cresce la coda viene ignorata, se si riduce l’entry viene completata con
zeri. In entrambi i casi l’archivio resta valido e ripristinabile, e il file
interessato viene segnalato:

```text
[WARN] file changed while it was archived; stored the size seen at open time path="/var/lib/docker/volumes/app/_data/wal.log" size=33554432
[WARN] backup completed with source anomalies changed_files=2 skipped_entries=1
```

Un socket unix trovato nella sorgente viene saltato con un avviso: non è
rappresentabile in un archivio tar e non contiene stato. FIFO, device e
attributi estesi vengono invece archiviati e ripristinati.

### Cosa viene preservato

Il round trip riproduce contenuto, permessi (inclusi setuid/setgid/sticky),
proprietario e gruppo, mtime al nanosecondo, symlink, **gruppi** di hard link,
FIFO, attributi estesi (quindi ACL POSIX e capabilities dei file) e i buchi dei
file sparsi. Gli header sono sempre in formato PAX: il formato USTAR
tronterebbe i timestamp al secondo.

Limiti noti: `archive/tar` non sa scrivere estensioni sparse, quindi l’archivio
di un file sparso è grande quanto la sua dimensione apparente (i buchi vengono
ricreati durante il restore); il ripristino di proprietario, device e attributi
`security.*`/`system.*` richiede i privilegi corrispondenti. Il default è
stretto: se FIFO, device, xattr, ACL o capability non possono essere ricreati,
il restore fallisce e il volume live resta invariato. Solo l’opt-in globale
`--best-effort-metadata` converte questi errori in warning e permette un restore
consapevolmente non fedele.

Anche in lettura gli attributi estesi non vengono mai persi in silenzio: se il
set di attributi cambia fra la chiamata che ne misura la dimensione e quella
che li legge, la lettura viene ripetuta; un attributo che il processo non ha il
diritto di leggere produce un warning con il nome dell’attributo e il percorso,
non una rimozione silenziosa dall’archivio. Un filesystem senza supporto xattr
resta invece un caso normale e non produce rumore.

In caso di crash durante il backup:

- il volume sorgente non viene modificato;
- un archivio temporaneo incompleto non viene pubblicato;
- gli archivi già pubblicati restano validi;
- il file temporaneo abbandonato viene rimosso al backup successivo.

Se il file finale è già visibile ma il `close` o la sincronizzazione della
directory falliscono, l’errore riporta esplicitamente il percorso come
“published but durability confirmation failed”. Non ripetere alla cieca:
eseguire `docker dvol verify --archive <percorso>` prima di decidere se rifare il
backup.

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
collision-free e ogni sostituzione è registrata in un journal sotto
`<root>/state/replacements`. All’avvio successivo dvol ripara le operazioni
interrotte: se la nuova directory era già stata pubblicata rimuove la copia
vecchia, altrimenti rimette al suo posto quella precedente, e in ogni caso
elimina lo staging rimasto. Il journal copre qualunque destinazione, compresi i
mountpoint Docker fuori dal repository, e i record ancora in uso da un altro
processo vengono saltati: due comandi dvol contemporanei non si disturbano. I
record dei bundle condividono inoltre un marker di commit durevole, così il
recupero prende una sola decisione per l’intero gruppo.

La riparazione parte a ogni avvio, quindi anche un comando innocuo come
`docker dvol backups` apre il journal mentre un restore è in corso. Il record
viene creato e bloccato con un protocollo che tollera questa corsa: chi crea il
record riprova con un altro nome se nel frattempo una riparazione se l’è preso,
e la riparazione ignora un record che il proprietario ha nel frattempo
completato e rimosso. Un secondo processo dvol non può quindi né far fallire un
restore in corso né fallire a sua volta.

Il risultato è che il dato non può sparire: dopo un restore interrotto il volume
contiene o il contenuto precedente o quello ripristinato, mai nulla.

## Permessi

Su Linux, i mountpoint dei volumi Docker sono spesso accessibili solo a root.
Se compare `permission denied`, usare un account autorizzato e mantenere il
repository degli archivi accessibile allo stesso account:

```bash
sudo docker dvol --root /srv/docker-volume-backups backup app
```

La lettura usa `O_NOATIME`, che il kernel concede solo al proprietario del file
o a chi ha `CAP_FOWNER`, e che alcuni filesystem di rete rifiutano. In quel caso
il backup si ferma:

```text
docker dvol: source cannot be read without updating access times (operation not
permitted); pass --allow-atime to accept access-time updates on the source
```

`--allow-atime` prosegue aggiornando gli atime della sorgente, con un avviso per
ogni entry interessata. È l’unico modo in cui un backup può modificare qualcosa
nel volume, e va richiesto esplicitamente.

Gli archivi vengono creati `0644`. Per backup che contengono dati sensibili:

```bash
docker dvol --archive-mode 0600 backup app
```

## Sviluppo e test

```bash
cd /percorso/del/repository/docker-volume-plugin
make build
make check                 # go vet + go test + go test -race + build
make test
make test-race
make lint
make e2e
sudo make acceptance
sudo make acceptance-defects
sudo make acceptance-mongo
make acceptance-stream
make clean
```

`make check` è il gate da superare prima di ogni commit; `sudo make acceptance`
prima di ogni modifica che tocchi backup, restore o il contratto della CLI.
Architettura, invarianti e regole per i test sono in `AGENTS.md`; gli audit da
cui derivano sono in `REVIEW.md` (2026-08-08), `QA_REVIEW_2026-08-11.md` e
`REVIEW_2026-09-11.md`.

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

Le immagini applicative della acceptance hanno tag di patch fissati
(`postgres:16.14-bookworm`, `mongo:7.0.39-jammy`,
`odoo:17.0-20260723`, `rclone/rclone:1.74.2`); le variabili
`POSTGRES_IMAGE`, `MONGO_IMAGE`, `ODOO_IMAGE` e `RCLONE_IMAGE` consentono una
matrice esplicita senza reintrodurre tag mobili. Lo scenario fidelity richiede
`getfattr` e fallisce se non riesce realmente a creare l’xattr di fixture.

`make acceptance-stream` esegue la matrice stdin/stdout attraverso `docker
dvol` e l’immagine Docker ufficiale `rclone/rclone:1.74.2`, usando una directory
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

`make acceptance-defects` esegue uno scenario per ciascun difetto emerso
dall’audit: bundle passato al restore singolo, backup a caldo sotto scrittura
continua, FIFO e socket nel volume, restore di più volumi dopo la cancellazione
di uno, ricerca del bundle Compose con `--timestamp` e con `--archive-name`,
disallineamento fra volumi dichiarati e archiviati, restore su volume montato,
restore interrotto e riparato, fedeltà completa dei metadati e `verify`.

I test unitari coprono backup read-only con verifica di contenuto, ownership,
permessi, mtime al nanosecondo, atime, symlink, hardlink, FIFO, attributi estesi
e file sparsi; destinazioni non sicure, errori di writer, stream troncati e
processo terminato durante il publish; restore atomico, rollback, journal di
recupero, compressione, bundle Compose, interruzioni e gestione di mountpoint
Docker. La suite di concorrenza (`make test-race`) esercita backup simultanei
sullo stesso nome di archivio, listing durante un backup in corso, restore
paralleli e recupero concorrente. `make acceptance-stream` ripete inoltre le
verifiche di invariabilità sui volumi Docker reali durante backup singoli,
bundle e Compose.

### API Go legacy

`Store.Create`, `Store.Get`, `Store.List`, `Store.Size`, `Store.Remove` e
`Store.VolumePath` descrivono il vecchio modello di volumi gestiti dentro il
repository e sono deprecati. La CLI non li usa per il lifecycle Docker: scopre
i volumi con Docker e chiama le API `BackupSource*`/`RestoreSource*` sui
mountpoint reali. `SizeWithError` resta disponibile agli embedder legacy e,
diversamente da `Size`, non nasconde errori di traversal.

## Layout

```text
cmd/dvol-cli-plugin/   plugin del client Docker (`docker dvol`)
pkg/dvol/              archivio backup/restore e parsing Compose
test/                  smoke test
Makefile               build, test, lint e installazione
```
