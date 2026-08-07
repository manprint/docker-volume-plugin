# User test scenario

Creare nella cartella di test i file necessari per eseguire i seguenti scenari di test e2e

## Alla fine dei test, rimuovere immagini scaricate, container e volumi usati per i test. Lasciare l'host pulito da quello creato in test.

### Scenario 1

1. creare un database postgres con un volume montato.
2. creare dei dati di esempio bel database
3. eseguire il backup del volume
4. distruggere il volume originario ed anche il container
5. ripristinare il volume
6. ricreare il container
7. verificare che TUTTI i dati nel database siano presenti

### Scenario 1.1

Ripetere lo scenario 1, con tutti i tipi di compressione disponibili

### Scenario 2

1. creare un docker compose per avviare un istanza di odoo
2. avviare il compose, con i volumi montati per il filestore di odoo e per il database
3. creare un db di test di odoo e verificare che odoo sia up and running, disponibile ed utilizzabile, con un db di prova (demo data), aggiungere qualche dato target da verificare sia in db che in filestore.
4. eseguire il backup dei volumi con la funzionalità di compose backup (archivio unico)
5. distruggere tutto con compose down -v
6. ripristinare i volumi
7. far risalire odoo e ricreare lo stack.
8. verificare la disponibilità di odoo ed i dati target.

### Scenario 2.1

Ripetere lo scenario 2, con tutti i tipi di compressione disponibili