# Specifica del prodotto

`dvol` è un Docker CLI plugin installato come `docker dvol`. Non aggiunge
sottocomandi a `docker volume` o `docker compose`, perché il client Docker non
permette ai plugin di estendere quei namespace builtin.

## Comandi supportati

```text
docker dvol list
docker dvol backups <volume>
docker dvol lock [--image immagine] <volume>
docker dvol unlock <volume>
docker dvol backup [opzioni] <volume>...
docker dvol restore [opzioni] <volume>... [archivio]
docker dvol verify <volume> [archivio]
docker dvol verify --archive <percorso>
docker dvol compose-backup -f compose.yaml [-p|--project-name progetto]
docker dvol compose-restore -f compose.yaml [-p|--project-name progetto] [archivio]
```

Il backup di più volumi senza `--bundle` produce un archivio separato per ogni
volume. Con `--bundle`, e sempre per `compose-backup`, viene prodotto un solo
archivio con `manifest.json` come prima entry e una directory top-level per
volume.

## Contratto operativo

- Piattaforma supportata: Linux, Docker Engine locale, Docker CLI >= 19.03.
- Driver supportato: `local` senza opzioni oppure bind-backed locale con
  `type=none`, `device` assoluto e `o=bind[,rw]`; driver remoti o di terze parti
  vengono rifiutati.
- La configurazione del driver non è archiviata: un target mancante viene creato
  come normale volume `local`; un target bind-backed va ricreato prima del
  restore quando si vuole conservare quel mapping host.
- Backup read-only: la sorgente non viene modificata salvo opt-in
  `--allow-atime` quando `O_NOATIME` non è disponibile.
- Pubblicazione backup: file temporaneo privato, sync, link/rename atomico e
  sync della directory. Un errore successivo alla pubblicazione è tipizzato e
  riporta il percorso completo da verificare.
- File di controllo: indice, lock e temporanei stanno dentro la directory degli
  archivi. dvol non scrive nulla nella directory padre della destinazione, che
  può quindi essere di sola lettura.
- Client Docker: dvol interpreta solo lo standard output del comando `docker`;
  gli avvisi su standard error non alterano la decodifica.
- Restore: estrazione e validazione complete in staging; metadati stretti per
  default, con `--best-effort-metadata` come opt-in esplicito.
- Restore bundle: tutti gli staging sono preparati prima degli swap e un journal
  di gruppo garantisce commit o rollback dell’intero insieme.
- Volumi mancanti creati dal restore: rimossi dopo un fallimento solo se ancora
  vuoti, inutilizzati e con mountpoint invariato.
- Volumi montati da container attivi: rifiutati salvo `--force`, con controllo
  iniziale e immediatamente precedente al commit.
- Lock volume: un container guardiano running, read-only e senza rete mantiene
  una reference Docker al volume. Il daemon rifiuta `volume rm`, anche con
  `--force`, prune e la rimozione tentata da `compose down -v`. Il guard dvol è
  escluso dal controllo dei container applicativi. `unlock` verifica etichette
  e mount prima di rimuoverlo e non rimuove mai il volume.
- Il lock è una protezione operativa, non un controllo di autorizzazione contro
  un amministratore del daemon. Compose può segnalare “Resource is still in
  use” mantenendo comunque exit code 0.

## Compose

La precedenza del nome progetto è:

1. `-p` / `--project-name`;
2. `COMPOSE_PROJECT_NAME`;
3. campo top-level `name:`;
4. nome normalizzato della directory del file Compose.

È accettato un solo file `-f`. dvol non implementa merge, interpolazione o
profili Compose: per quei casi occorre passare l’output materializzato di
`docker compose config`.

## Toolchain

Go 1.27.3, fissato dalla direttiva `go` in `go.mod` e usato anche dalla CI
tramite `go-version-file`. Dipendenze dirette: `klauspost/compress`,
`klauspost/pgzip`, `pierrec/lz4/v4`, `ulikunitz/xz`, `golang.org/x/sys`,
`gopkg.in/yaml.v3`.

## Fuori scope

- snapshot point-in-time di applicazioni in scrittura;
- Docker Desktop o daemon remoto;
- retention/prune automatico degli archivi;
- gestione di volumi tramite un Docker Engine managed plugin.

La documentazione operativa completa, incluse matrice Docker e procedure QA, è
in `README.md`; gli invarianti di implementazione sono in `AGENTS.md`.
