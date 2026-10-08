# Credenziali temporanee per un agente, con il tuo utente

Implementazione Go per GitHub.com e GitLab.com, Linux/macOS (amd64 e arm64).
Nessuna dipendenza Go esterna. Per compilare: Go 1.23 o successivo, preferibilmente
la release stabile aggiornata. Servono `git`, `gh` e `glab` nel PATH.

```bash
git clone https://github.com/nolith/ai-session-auth.git
cd ai-session-auth
go build -o agent-auth .
```

Il binario è indipendente dal runtime Go.

Avvia un harness con accesso API e Git HTTPS usando la tua identità:

```bash
./agent-auth --config config.json run -- codex
```

Puoi sostituire `codex` con qualsiasi comando e i suoi argomenti.
Non sono necessarie chiavi SSH per i remote GitHub/GitLab standard.

## Cosa è implementato

- GitHub App **user access token**, non installation token/bot.
- Autorizzazione iniziale con device flow; non serve client secret né chiave RSA.
- Rinnovo GitHub su richiesta, cinque minuti prima della scadenza.
- PAT GitLab **personale fine-grained** nuovo per ogni sessione.
- Progetti GitLab selezionati e nessun permesso PAT-management nel token dell'agente.
- Verifica del nome utente su entrambi i provider prima di avviare l'harness.
- Wrapper `gh` e `glab` che leggono il token corrente e invocano le CLI originali.
- Git credential helper che controlla protocollo, hostname e percorso del repository.
- Riscrittura temporanea dei remote SSH standard in HTTPS tramite configurazione di processo.
- Revoca del PAT GitLab all'uscita normale, su Ctrl-C/SIGTERM o al limite di durata.
- Stato GitHub scritto atomicamente, file segreti chmod 600 e lock tra sessioni.
- Nessun token in output ordinario, argv o URL Git.

## Setup GitHub, una volta

1. Apri https://github.com/settings/apps e registra una GitHub App.
2. Disabilita i webhook, che questo client non usa. Non servono servizi pubblici.
3. Abilita **Device flow** e mantieni abilitata la scadenza degli user access token.
4. Configura questi repository permissions:

| Permesso | Livello |
|---|---|
| Contents | Read and write |
| Issues | Read and write |
| Pull requests | Read and write |
| Actions | Read and write |
| Checks | Read-only |
| Commit statuses | Read-only |
| Metadata | Read-only, automatico |

Se vuoi modificare i file `.github/workflows/`, aggiungi **Workflows: write**.
Non aggiungere Administration, Secrets o privilegi di bypass delle protezioni.

5. Installa la App sul tuo account/organizzazione scegliendo i repository interessati.
   Per un'organizzazione possono servire approvazione dell'amministratore e SSO attivo.
6. Copia il **Client ID** (non l'App ID) nel file di configurazione.
7. Imposta `expected_user` sul tuo login GitHub.

Gli user token consentono le operazioni che sia il tuo utente sia la App possono fare.
L'identità autore dei commit dipende anche da `git config user.name/user.email`:
il token determina l'utente che esegue il push, non riscrive l'autore del commit.

## Setup GitLab.com, una volta

Apri https://gitlab.com/-/user_settings/personal_access_tokens.
Crea un PAT **emittente** per il tuo utente, distinto dai PAT destinati all'agente.

La soluzione preferita è un PAT emittente fine-grained che abbia:

- Nel boundary **User**: Personal Access Token: **Create, Read, Revoke**;
  User: **Read** per verificare l'identità.
- Nei progetti selezionati: almeno tutti i permessi del token dell'agente,
  indicati in `gitlab.granular_scopes` in `config.example.json`.

Nomi API dei privilegi dell'emittente nel boundary User:

```json
["create_personal_access_token", "read_personal_access_token", "revoke_personal_access_token", "read_user"]
```

Un token emittente fine-grained può creare solo PAT con permessi e confini
uguali o più limitati ai propri. Avere solo Create PAT non basta per emettere
un token capace di fare push. Read e Revoke servono alla pulizia della sessione.

Come alternativa di bootstrap puoi usare un tuo PAT legacy con scope `api`.
In entrambi i casi il token consegnato all'agente viene creato **fine-grained**.
Il PAT emittente resta soggetto alla propria scadenza e alle policy di GitLab.com;
questa versione non rinnova automaticamente la credenziale emittente.

Recupera l'ID numerico di ciascun progetto e aggiorna tutti e tre:

- `gitlab.repositories`: percorsi esatti, per esempio `nolith/project`;
- `gitlab.project_ids`: ID numerici;
- `resourceIds`: `gid://gitlab/Project/ID` nei granular scopes.

Il launcher interroga i progetti e controlla che ID e percorsi coincidano.
Il profilo incluso copre codice, issue/commenti, creazione/modifica MR, pipeline,
job e artifact in lettura. I privilegi distruttivi, amministrativi e di merge
sono esclusi dal profilo iniziale. Git push resta governato dai tuoi privilegi
e dalle branch protection del progetto.

## Configurazione e avvio

```bash
cp config.example.json config.json
# Modifica Client ID, username, repository e ID GitLab prima di continuare.
./agent-auth --config config.json save-gitlab-issuer
./agent-auth --config config.json github-login
./agent-auth --config config.json run -- codex
```

`save-gitlab-issuer` chiede il PAT con input nascosto; non metterlo nel comando
o nel file config. Lo salva nel percorso `issuer_token_file`, con mode 600.
`github-login` mostra URL e codice monouso da autorizzare nel browser; non stampa
i token ricevuti. Lo stato viene salvato nel percorso `state_file`.

Per avviare l'harness da un checkout diverso, passa percorsi assoluti:

```bash
/path/ai-session-auth/agent-auth \
  --config /path/ai-session-auth/config.json run -- codex
```

La directory corrente e gli argomenti del comando vengono conservati.
Per provare solo un provider imposta `enabled: false` per l'altro.

## Verifica sul tuo account

Prima della sessione operativa puoi aprire una shell di verifica:

```bash
./agent-auth --config config.json run -- bash
```

Dentro la shell, verifica l'identità e operazioni di sola lettura:

```bash
gh api user --jq .login
glab api user
gh pr list --repo YOUR_GITHUB_USERNAME/YOUR_REPOSITORY
glab mr list --repo YOUR_NAMESPACE/YOUR_PROJECT
glab ci list --repo YOUR_NAMESPACE/YOUR_PROJECT
git ls-remote origin
exit
```

La creazione del PAT GitLab avviene realmente anche per questa verifica e il PAT
viene revocato all'uscita. Non stampa le credenziali in questi comandi.
La verifica completa di push, commenti, creazione MR/PR e controllo pipeline
va eseguita su un repository di prova che scegli tu.

## Durata e comportamento delle sessioni

`session_hours` è 24 per default, massimo 48. La scadenza GitLab è arrotondata
alla mezzanotte UTC successiva alla fine prevista della sessione. Quindi:

| Sessione richiesta | Durata di fallback del PAT GitLab |
|---|---|
| 24 ore | Da 24 a meno di 48 ore |
| 48 ore | Da 48 a meno di 72 ore |

L'arrotondamento evita che il token scada prima della fine della sessione.
Normalmente viene revocato quando l'harness termina; se il computer si spegne,
il launcher è ucciso con SIGKILL o la rete non funziona, scade alla data prevista.
Il broker smette di fornire token al limite configurato e il launcher termina
il gruppo di processi dell'harness. Processi che si staccano deliberatamente
dal gruppo non possono essere garantiti come terminati.

GitHub usa token da otto ore e rinnova il refresh token nello stato persistente.
Sessioni parallele della stessa App/utente condividono la credenziale GitHub
corrente; ogni wrapper la recupera nuovamente. Non si revoca GitHub all'uscita,
per non invalidare altre sessioni o il refresh token: l'access token scade.
Una copia di quel token rimane valida fino alla sua scadenza effettiva.
Un comando `gh` già avviato non può cambiare token durante la propria esecuzione;
per watch lunghi che attraversano la scadenza bisogna rilanciare il comando.

## Confini della prima versione

Questo launcher gestisce il ciclo delle credenziali, **non è una sandbox**.
Il PAT emittente e il refresh token non sono passati all'harness, ma se l'agente
gira con il tuo stesso utente e può leggere liberamente la tua home, può leggere
i file segreti chmod 600. Per l'isolamento servono un utente distinto, una sandbox
che nasconda quei percorsi o un broker esterno. Il socket di sessione consente
di ottenere i token temporanei: è intenzionale.

L'allowlist del credential helper limita dove il helper restituisce token;
non limita l'uso API delle CLI. GitLab applica server-side i progetti nei granular
scopes. GitHub applica server-side i repository accessibili alla App: la lista
`github.repositories` di questa versione limita Git, non riduce ulteriormente
l'accesso API del token. Installa la App solo sui repository necessari.

I wrapper richiedono che l'harness erediti PATH, `AI_AUTH_SOCKET` e `GIT_CONFIG_*`.
Un container senza socket montato o una shell che ripulisce queste variabili
richiede adattamento. Invocare `/usr/bin/gh` direttamente salta il wrapper.
I config CLI sono temporanei, per evitare il login personale preesistente;
alias/extension e preferenze personali non sono copiati automaticamente.

La riscrittura copre `git@github.com:owner/repo.git`, `ssh://git@github.com/...`
e i corrispondenti remote GitLab.com. Alias SSH e host/porte personalizzati
non sono supportati. La configurazione Git sul disco non viene modificata.
I remote devono essere privi di token/password incorporati nell'URL.
La firma dei commit SSH/GPG rimane una configurazione separata.

Gli scope inclusi sono un profilo iniziale basato sulla documentazione corrente.
Alcuni comandi glab/gh fanno query accessorie e possono richiedere permessi
aggiuntivi: un 403 non comporta ampliamento automatico dei privilegi.
Git LFS e harness con comportamento speciale di terminale vanno verificati
localmente. Un crash durante la rotazione GitHub dopo l'emissione ma prima
del salvataggio può richiedere una nuova autorizzazione device.

## Test

```bash
go test -race ./...
go vet ./...
```

I test coprono identità, scadenze UTC, rinnovo concorrente e lock cancellabile,
persistenza dei segreti, creazione/revoca del PAT, corrispondenza progetti,
allowlist, protocollo Git credential, riscrittura dei remote tramite Git reale,
RPC su socket Unix, exit code, timeout del gruppo di processi e input cancellabile.
Le API dei provider sono simulate: nessun test usa credenziali reali.
La CI esegue questi controlli su Linux e macOS con Go stabile.

Nel runtime di creazione è vietata la creazione di socket Unix: il relativo test
è saltato solo in caso di EPERM/EACCES. Il launcher richiede un ambiente che
consenta socket Unix. I flussi autenticati reali richiedono il setup descritto
sopra e non sono stati eseguiti sui tuoi account.

Lo stato GitHub e il formato config sono compatibili con il precedente launcher
Python: puoi riusare i file esistenti. Non mettere stato OAuth o PAT nel repository.

## Fonti tecniche

- https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app
- https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens
- https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens/
- https://docs.gitlab.com/api/graphql/reference/experimental/input_objects/#personalaccesstokencreateinput
- https://gitlab.com/gitlab-org/gitlab/-/merge_requests/228600
- https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens_rest/
- https://docs.gitlab.com/auth/tokens/fine_grained_access_tokens_other/
- https://git-scm.com/docs/gitcredentials
