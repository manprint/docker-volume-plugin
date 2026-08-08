# Analisi STDIN/STDOUT per backup e restore

## Stato

Documento di analisi e proposta: in questa fase non viene implementato alcun
cambiamento. Il nome segue la richiesta STDIN_SDTOUT_ANALYSYS.md. Nel testo
si usa --stdout (l’esempio iniziale usa --stout).

## Verifica di rclone

È stata usata l’immagine ufficiale rclone/rclone:latest, versione rilevata
v1.75.0, con una directory locale montata nel container come destinazione
remota simulata.

La help di rclone e i test reali confermano:

- rclone rcat remote:path/to/file legge stdin e scrive un solo oggetto;
- il percorso completo dell’oggetto è obbligatorio;
- una directory non basta e causa errore;
- un oggetto esistente viene sovrascritto;
- input piccoli possono essere accumulati in RAM;
- input grandi possono usare chunk/multipart secondo il backend;
- --size deve essere esatto e non è normalmente noto prima della compressione;
- il trasferimento completo non è in generale ritentabile da rcat, perché i byte
  già letti da stdin non sono conservati;
- rclone cat remote:path/to/file produce stdout ed è adatto a un restore da pipe.

La documentazione web ufficiale non è stata usata come unica fonte perché in
questo ambiente ha risposto con HTTP 403; le proprietà sopra sono quelle della
help locale e delle esecuzioni dell’immagine Docker.

## Principi proposti

1. dvol resta indipendente da rclone e produce/consuma byte su pipe.
2. In modalità stream stdout contiene solo l’archivio; stato ed errori vanno su
   stderr.
3. Un volume produce un archivio singolo.
4. Più volumi producono un bundle unico con manifest.json, non una
   concatenazione di archivi.
5. Il restore valida tutto e usa staging prima di modificare i volumi.
6. La fotografia della sorgente e la pubblicazione atomica remota non sono
   garantite dal solo trasporto via pipe.

---

## 1. Mia domanda: è possibile gestire backup multipli di volumi con uno stream?

### Fattibilità

Sì, ma non concatenando gli output di più backup singoli: una concatenazione non
definisce confini, nomi Docker, manifest o rollback.

La soluzione corretta è un archivio unico:

    manifest.json
    volume-a/...
    volume-b/...

Il progetto possiede già un bundle con manifest.json per Compose. Propongo di
riusarlo anche per backup diretto multi-volume:

- un volume: archivio singolo;
- due o più volumi: bundle;
- ordine deterministico dei volumi;
- restore completo o sottoinsieme esplicito;
- --same-destination resta una regola per file locali.

### Esempi e2e di funzionamento file

    docker dvol backup --stdout --compression zstd mongo_data |
      rclone rcat "myremote:/dumps/mongo/mongo_data.tar.zst"

    docker dvol backup --stdout --compression zstd pg_data filestore_data |
      rclone rcat "myremote:/dumps/odoo/odoo-volumes.tar.zst"

Il bundle contiene manifest.json, pg_data/... e filestore_data/....

### Possibili criticità

- Un bundle è un solo oggetto: anche un restore selettivo deve leggerlo e
  validarlo.
- Nessun volume deve essere pubblicato prima della validazione completa.
- Nomi e path del manifest devono essere validati.
- Una chiusura anticipata del consumer deve generare errore.
- Il bundle non risolve modifiche concorrenti del container.

### Proposte alternative migliori

Se servono retry e lifecycle indipendenti, eseguire un comando per volume:

    for volume in pg_data filestore_data; do
      docker dvol backup --stdout --compression zstd "$volume" |
        rclone rcat "myremote:/dumps/odoo/$volume.tar.zst"
    done

Per consistenza applicativa, precedere il bundle da quiescenza o snapshot
esterno: lo stream risolve il trasporto, non la fotografia dei dati.

### Risposte e considerazioni dell’utente

- [x] Confermi un bundle unico con più volumi?  ---> CONFERMO
- [x] Bundle automatico o flag esplicito --bundle? ---> FLAG ESPLICITO
- [x] Vuoi restore selettivo? ---> NO, VA BENE RIPRISTINARE TUTTO IL BUNDLE
- Considerazioni: TUTTO QUELLO IMPLEMENTATO VA VALIDATO SOLIDAMENTE CON TEST E2E SULLE MATRICE PRESENTI, ANCHE ESTESO
---

## 2. Mia domanda: devo per forza specificare il nome lato rclone?

### Fattibilità

Sì. rcat richiede:

    rclone rcat remote:path/to/file

Il producer controlla i byte ma non il nome dell’oggetto. Una directory non
genera automaticamente un nome e un oggetto esistente viene sovrascritto.

### Esempi e2e di funzionamento file

    docker dvol backup --stdout --compression zstd mongo_data |
      rclone rcat "myremote:/dumps/mongo/latest.tar.zst"

Il nome fisso implica sovrascrittura intenzionale. Un nome univoco può essere
scelto dal chiamante:

    id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    docker dvol backup --stdout --compression zstd mongo_data |
      rclone rcat "myremote:/dumps/mongo/$id.tar.zst"

--archive-name controlla oggi il file locale. Con stdout non può rinominare
l’oggetto remoto perché tra dvol e rclone passano solo byte.

### Possibili criticità

- --archive-name potrebbe essere frainteso come nome remoto.
- Un nome fisso può sovrascrivere un backup valido.
- Un’estensione errata rende meno riconoscibile il formato.
- Stato su stdout corromperebbe l’archivio binario.

### Proposte alternative migliori

Lasciare a rclone il nome e documentare che il percorso è obbligatorio.
Eventualmente aggiungere --print-suggested-name, sempre su stderr. Non consiglio
di integrare direttamente rclone nella CLI dvol.

### Risposte e considerazioni dell’utente

- [X] Confermi il percorso completo obbligatorio? ---> CONFERMO
- [x] Vuoi un nome suggerito su stderr? ---> NO
- [X] Su collisione: errore, overwrite esplicito o entrambe le modalità? ---> ENTRAMBE
- Considerazioni: TUTTO QUELLO IMPLEMENTATO VA VALIDATO SOLIDAMENTE CON TEST E2E SULLE MATRICE PRESENTI, ANCHE ESTESO

---

## 3. Mia domanda: si possono gestire automaticamente i timestamp omettendo il nome?

### Fattibilità

Il timestamp può essere generato automaticamente, ma rcat deve comunque ricevere
un percorso oggetto. In uno stream il nome non viene trasmesso al ricevente.

--timestamp resta utile per file locali e nome suggerito; il nome remoto deve
essere costruito dalla shell o da uno script.

### Esempi e2e di funzionamento file

    stamp="$(date -u +%d%m%Y_%H%M%S)"
    docker dvol backup --stdout --timestamp --compression zstd mongo_data |
      rclone rcat "myremote:/dumps/mongo/mongodb-vol_$stamp.tar.zst"

    set -o pipefail
    docker dvol backup --stdout --compression zstd mongo_data |
      rclone rcat "myremote:/dumps/mongo/mongodb-vol_$stamp.tar.zst"

### Possibili criticità

- Va scelto UTC oppure timezone locale.
- La precisione al secondo non evita collisioni.
- Un oggetto fallito può avere un nome apparentemente valido.
- --timestamp non crea directory né nomina l’oggetto rclone.

### Proposte alternative migliori

Consiglio timestamp UTC e, quando serve unicità, timestamp più suffisso casuale
o identificatore di esecuzione. Per dati critici usare un nome temporaneo e una
promozione successiva.

### Risposte e considerazioni dell’utente

- [X] Timestamp UTC o timezone locale? --> ENTRAMBE LE POSSIBILITA
- [X] Formato: ddmmaaaa_hhmmss, ISO 8601 o altro? VA BENE ddmmaaaa_hhmmss
- [ ] Collisione: errore oppure overwrite? ERRORE 
- Considerazioni: TUTTO QUELLO IMPLEMENTATO VA VALIDATO SOLIDAMENTE CON TEST E2E SULLE MATRICE PRESENTI, ANCHE ESTESO

---

## 4. Mia domanda: quali altri casi non sono gestibili con questo modello?

### Fattibilità

Sono adatti: backup singolo, bundle multi-volume, Compose bundle, restore di un
oggetto noto e pipeline verso rclone, SSH, cifratura o altri programmi Unix.

Non sono automatici con il solo stdout:

- elenco, latest e retention degli oggetti remoti;
- indice locale e organizzazione per directory;
- retry completo dopo disconnessione;
- deduplicazione, versioning e lifecycle remoto;
- checksum remoto se il backend non espone un hash compatibile;
- snapshot o quiescenza della sorgente.

### Esempi e2e di funzionamento file

    docker dvol backups mongo_data
    rclone lsf "myremote:/dumps/mongo/"

    rclone cat "myremote:/dumps/mongo/mongo.tar.zst" |
      docker dvol restore --stdin mongo_data

### Possibili criticità

- Un oggetto parziale può sembrare valido guardando solo il nome.
- rcat non può riprendere una pipe già consumata.
- La retention remota non è la retention locale.
- stdout binario richiede stderr per ogni messaggio.
- Il consumer può esercitare backpressure e richiedere timeout esterni.

### Proposte alternative migliori

Documentare due profili:

1. stream diretto, con minimo spazio locale;
2. spool locale atomico seguito da rclone copyto/move, con retry e verifica.

Un compromesso è usare un oggetto in .partial/, verificarlo e promuoverlo con
rclone moveto. L’atomicità della promozione dipende dal backend.

### Risposte e considerazioni dell’utente

- [x] Vuoi un indice/manifest esterno per i backup remoti? -> NO
- [X] Accetti che list/latest/retention siano responsabilità di rclone o script? ---> ACCETTO
- [X] Vuoi documentare stream e spool? -> SI
- Considerazioni: TUTTO QUELLO IMPLEMENTATO VA VALIDATO SOLIDAMENTE CON TEST E2E SULLE MATRICE PRESENTI, ANCHE ESTESO

---

## 5. Mia domanda: possiamo gestire anche tutta la parte Docker Compose?

### Fattibilità

Sì. compose-backup --stdout è il caso naturale per un bundle unico, perché la
modalità Compose possiede già manifest.json.

Proposta:

- compose-backup --stdout produce un bundle;
- il manifest conserva volumi, formato e timestamp;
- compose-restore --stdin consuma il bundle;
- rclone resta un transport esterno.

### Esempi e2e di funzionamento file

    stamp="$(date -u +%d%m%Y_%H%M%S)"
    docker dvol compose-backup -f compose.yml -p mongo \
      --stdout --compression zstd |
      rclone rcat "myremote:/dumps/mongo/mongo_$stamp.tar.zst"

    rclone cat "myremote:/dumps/mongo/mongo_08082026_120000.tar.zst" |
      docker dvol compose-restore --stdin -f compose.yml -p mongo

Bundle logico:

    manifest.json
    mongo_data/...
    mongo_logs/...

### Possibili criticità

- Il Compose file può cambiare tra backup e restore.
- Il progetto o i nomi dei volumi possono cambiare.
- Il manifest può contenere volumi non più presenti.
- Un restore selettivo può non essere coerente con l’applicazione.
- Volumi esterni o driver diversi richiedono una politica esplicita.

### Proposte alternative migliori

Il manifest deve essere auto-descrittivo e il Compose file servire a risolvere
i target locali. compose-restore dovrebbe confrontarli prima di modificare dati
e fallire in caso di mismatch, salvo mapping esplicito.

Compose non congela i processi: per consistenza applicativa occorre quiescenza
o stop dei servizi.

### Risposte e considerazioni dell’utente

- [X] Confermi compose-backup --stdout come bundle unico? -> SI
- [X] compose-restore --stdin richiede sempre -f e -p? --> NON SO RISPONDERE, FAI LA COSA MIGLIORE E DOCUMENTA
- [X] Confronto rigido tra manifest e Compose file? ---> NON SO RISPONDERE, VALUTA TU E DOCUMENTA
- [x] Restore selettivo o solo completo? -> COMPLETO
- Considerazioni: TUTTO QUELLO IMPLEMENTATO VA VALIDATO SOLIDAMENTE CON TEST E2E SULLE MATRICE PRESENTI, ANCHE ESTESO

---

## 6. Mia domanda: possiamo fare restore da stdin recuperando i backup da rclone?

### Fattibilità

Sì:

    rclone cat "myremote:/dumps/mongo/mongo.tar.zst" |
      docker dvol restore --stdin mongo_data

    rclone cat "myremote:/dumps/odoo/odoo.tar.zst" |
      docker dvol restore --stdin pg_data filestore_data

    rclone cat "myremote:/dumps/mongo/mongo.tar.zst" |
      docker dvol compose-restore --stdin -f compose.yml -p mongo

### Esempi e2e di funzionamento file

Un input troncato deve fallire senza sostituire il volume:

    set -o pipefail
    rclone cat "myremote:/dumps/mongo/mongo.tar.zst" |
      docker dvol restore --stdin mongo_data

Il formato va rilevato dall’header di compressione e dal contenuto tar; non
deve dipendere solo dall’estensione del percorso remoto.

### Possibili criticità

- Estrarre nel volume live è pericoloso: un input troncato lascia dati parziali.
- Pubblicare un volume prima di validare gli altri rompe la consistenza.
- Indice, path e latest non si applicano a stdin.
- Errori di rclone o chiusure anticipate devono dare exit code non zero.
- Il restore può richiedere spazio temporaneo.

### Proposte alternative migliori

Prima versione sicura: spool locale privato.

1. leggere tutto stdin in un file temporaneo;
2. validare compressione, tar, manifest e contenuto;
3. usare il restore esistente con staging atomico;
4. rimuovere il temporaneo anche in errore.

Una versione successiva può estrarre in staging durante la lettura, ma deve
pubblicare solo dopo EOF valido. Per più volumi servono commit/rollback
coordinati. Il restore diretto nel volume live non è raccomandato.

### Risposte e considerazioni dell’utente

- [X] Confermi lo spool locale come prima implementazione? ---> VOGLIO 2 MODALITA: CON SPOOL E SENZA SPOOL, CON FLAG PER DECIDERE
- [x] Sintassi: restore --stdin VOLUME, restore VOLUME --stdin o anche -? ---> VALUTA TU LA MIGLIORE PER COERENZA
- [X] Bundle: restore completo di default o solo volumi espliciti? --> COMPLETO
- [X] Nessun volume prima della validazione completa? --> VALUTA TU LA MIGLIORE PER COERENZA
- Considerazioni: TUTTO QUELLO IMPLEMENTATO VA VALIDATO SOLIDAMENTE CON TEST E2E SULLE MATRICE PRESENTI, ANCHE ESTESO

---

## 7. Mia domanda: come gestiamo la consistenza del backup verso remoto?

### Fattibilità

Separare:

1. consistenza della sorgente Docker;
2. integrità dello stream prodotto;
3. pubblicazione e durabilità dell’oggetto remoto.

Lo stream risolve il trasporto, non tutti e tre i problemi.

### Esempi e2e di funzionamento file

Pipeline diretta:

    set -o pipefail
    docker dvol backup --stdout --compression zstd mongo_data |
      rclone rcat "myremote:/dumps/mongo/mongo_08082026_120000.tar.zst"

Pipeline con temporaneo e promozione:

    set -euo pipefail
    id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    tmp="myremote:/dumps/mongo/.partial/mongo_$id.tar.zst"
    final="myremote:/dumps/mongo/mongo_$id.tar.zst"

    docker dvol backup --stdout --compression zstd mongo_data |
      rclone rcat "$tmp"
    rclone size "$tmp"
    rclone moveto "$tmp" "$final"

Un eventuale checksum deve riferirsi ai byte compressi ricevuti da rclone, non ai
dati tar prima della compressione.

### Possibili criticità

- Read-only non significa snapshot: modifiche concorrenti possono produrre un
  archivio non coerente.
- Errori di lettura, compressione o pipe chiusa devono generare errore.
- Una rete interrotta può lasciare un oggetto parziale.
- rcat non può ritentare da capo senza ricreare l’input.
- L’atomicità di moveto dipende dal backend: può essere rename oppure
  copia+cancellazione.
- Upload riuscito non implica automaticamente replica o durabilità geografica.

### Proposte alternative migliori

Profilo di massima affidabilità:

1. quiescenza o snapshot esterno;
2. stream verso un oggetto univoco in .partial/;
3. checksum o verifica dimensionale;
4. successo completo dell’upload;
5. promozione al nome finale;
6. retention dei temporanei secondo policy;
7. versioning/immutability del backend quando richiesto.

Per backup critici:

    snapshot/quiescenza -> archivio locale atomico -> rclone copyto/move -> verifica

### Risposte e considerazioni dell’utente

- [X] Accetti la distinzione tra consistenza sorgente e integrità/durabilità remota? VEDI CONSIDERAZIONI
- [X] Vuoi un checksum dei byte compressi? VEDI CONSIDERAZIONI
- [X] Promozione .partial da script esterno o helper ufficiale? VEDI CONSIDERAZIONI
- [X] Vuoi una modalità safe remote con spool/promozione? VEDI CONSIDERAZIONI
- [X] Oggetti parziali rimossi o messi in quarantena? VEDI CONSIDERAZIONI
- Considerazioni: COME VERISONE V1, FAI LA COSA PIÙ SEMPLICE POSSIBILE, TESTA E DOCUMENTA

---

## Contratto CLI proposto da confermare

Backup singolo:

    docker dvol backup --stdout --compression zstd mongo_data |
      rclone rcat "remote:/path/mongo_data.tar.zst"

Backup multi-volume:

    docker dvol backup --stdout --compression zstd pg_data filestore_data |
      rclone rcat "remote:/path/odoo-volumes.tar.zst"

Backup Compose:

    docker dvol compose-backup --stdout -f compose.yml -p project |
      rclone rcat "remote:/path/project.tar.gz"

Restore:

    rclone cat "remote:/path/mongo_data.tar.zst" |
      docker dvol restore --stdin mongo_data

La proposta è rifiutare con errore chiaro:

- --stdout insieme a --destination;
- restore --stdin insieme a un path archivio;
- bundle senza mapping target sufficiente;
- qualsiasi messaggio di stato su stdout in modalità binaria.

##*** ULTERIORI NOTE: SPECIFICA IMPORTANTISSIMA DA RISPETTARE **##

NELLA MODALITÀ STEAM NON VOGLIO CONSUMARE SPAZIO SU DISCO PER TENERE FILE TEMPORANEI, PARTI, ECT.
QUESTA MODALITÀ MI SERVE SE AD ESEMPIO HO SOLO 2GB DI SPAZIO DISPONIBILE E DEVO BACKUPPARE UN VOLUME DA 20GB.


