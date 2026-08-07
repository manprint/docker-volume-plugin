# MongoDB E2E test matrix

La suite usa entrambi gli artefatti presenti in questa directory:

- `mongo-compose.yaml`, con i volumi `mongo_data` e `mongo_logs`;
- `script-start.mongo.sh`, che crea gli stessi volumi `local` bind-backed e avvia `mongodb`.

La suite completa si esegue dalla root del repository:

```bash
make build
sudo make acceptance-mongo
```

## Matrice combinatoria

Ogni riga viene eseguita per tutte le tre compressioni (`gzip`, `xz`, `zstd`).
Il totale è quindi 18 casi: 2 bootstrap × 3 compressioni × 3 target di restore.

| ID | Bootstrap | Target restore | Verifiche principali |
| --- | --- | --- | --- |
| M-C-E | Compose | stessi nomi, volumi già esistenti | sostituzione atomica, sentinel eliminato, Mongo riavviabile |
| M-C-M | Compose | nomi mancanti, ricreati da `docker dvol` | creazione volume `local`, dati DB e log ripristinati |
| M-C-R | Compose | nomi nuovi `mongo_*_copy_*` | restore su volumi rinominati e probe Mongo indipendente |
| M-S-E | script-start | stessi nomi, volumi già esistenti | sostituzione atomica, sentinel eliminato, Mongo riavviabile |
| M-S-M | script-start | nomi mancanti, ricreati da `docker dvol` | creazione volume `local`, dati DB e log ripristinati |
| M-S-R | script-start | nomi nuovi `mongo_*_copy_*` | restore su volumi rinominati e probe Mongo indipendente |

## Verifiche per ogni caso

### Bootstrap e applicazione

- directory bind-backed inizializzate quando usa Compose;
- volumi Docker individuati tramite `docker volume inspect`;
- container Mongo avviato dalla configurazione originale;
- readiness tramite `db.adminCommand({ping: 1})`;
- autenticazione root Mongo verificata;
- database `acceptance` e collection `records` create;
- tre record con payload noto inseriti e riletti;
- marker scritto nel volume dei log e verificato.

### Backup

- entrambi i volumi vengono backuppati con una singola invocazione multi-volume;
- viene eseguito anche il backup flat multi-volume con due mapping ripetibili:
  `mongo_data::mongo-flat-data` e `mongo_logs::mongo-flat-logs`;
- il timestamp è comune ai due archivi flat;
- gli archivi flat sono direttamente nella directory di destinazione, senza
  sottodirectory per volume;
- il file `.dvol-index.json` associa ogni archivio al volume Docker corretto;
- il file indice viene scritto atomicamente e ha permessi `0644`;
- gli archivi flat hanno permessi `0644` e non vengono sovrascritti in caso di
  collisione;
- `docker dvol backups <volume>` trova anche gli archivi flat tramite l’indice;
- `docker dvol restore <volume> <basename>` risolve correttamente un archivio
  flat tramite l’indice;
- Mongo resta avviato per mantenere montati i bind-backed volumes;
- `db.fsyncLock()` crea una finestra quiescente e applicativamente coerente;
- `docker pause` blocca anche i writer di background durante la lettura;
- il lock viene rimosso dopo il backup;
- archivi non vuoti e con estensione/formato atteso;
- archivio dati contiene `WiredTiger`;
- archivio log contiene `mongod.log` e `acceptance-marker.log`;
- il backup compare nell’elenco `docker dvol backups`;
- manifest sorgente prima/dopo backup identico;
- nessun file sorgente viene modificato.

### Restore su nomi esistenti

- i volumi vengono ricreati con un file sentinel estraneo;
- il restore sostituisce il volume, non esegue un merge;
- il sentinel viene eliminato;
- Mongo riparte con la configurazione originale;
- i record e il marker dei log sono ancora presenti.

### Restore su nomi mancanti/rinominati

- `docker dvol restore` crea automaticamente i volumi Docker mancanti;
- il target non dipende dai bind mount originali;
- un probe Mongo separato monta il volume dati ripristinato;
- il probe verifica count e payload dei record;
- il volume log ripristinato contiene il marker;
- il volume originale non è necessario per leggere il backup.

### Sicurezza e cleanup

- la suite rifiuta volumi `mongo_data`/`mongo_logs` già esistenti;
- rifiuta il container `mongodb` già esistente;
- rifiuta la porta TCP 27017 già occupata;
- usa una directory temporanea per ogni combinazione;
- rimuove container, volumi copiati, archivi temporanei e immagini scaricate dalla suite;
- un errore interrompe il caso senza trasformarlo in uno skip.

## Casi manuali aggiuntivi

| ID | Azione | Risultato atteso |
| --- | --- | --- |
| M-I-01 | interrompere il processo durante il backup | nessuna modifica ai volumi sorgente e nessun archivio finale parziale |
| M-I-02 | usare un archivio corrotto durante restore | volume target invariato |
| M-I-03 | creare un volume con lo stesso nome ma dati diversi | restore sostituisce i dati e rimuove il sentinel |
| M-I-04 | eseguire backup con Mongo attivo e scrivente | il comando è read-only, ma la coerenza applicativa non è garantita senza quiescenza |
