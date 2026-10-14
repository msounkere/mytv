# Processus de travail

- Branche `main` : toujours verte (`gofmt -l`, `go vet ./...`, `go test -race ./...`). Seul le **coordinateur** y fusionne.
- Chaque codeur travaille dans son worktree (`/opt/mytv-work/<codeur>`) sur une branche `feat/us-NN-nom`, jamais sur `main`.
- Petits commits. Message : `US-NN : ce qui change` + ligne vide + le pourquoi.
- Quand une US est prete : tests verts, puis ecrire le fichier `/opt/mytv-work/statut/<codeur>.<US>.pret` contenant la branche et le dernier commit, puis ATTENDRE `/opt/mytv-work/statut/<codeur>.<US>.verdict` (ecrit par le coordinateur : `FUSIONNE` ou `CORRIGER` + remarques de revue).
- Apres un verdict `FUSIONNE`, le codeur fait `git rebase main` avant l'US suivante.
- Les contrats (`internal/domain`) ne se modifient que par le coordinateur : demander par un fichier `/opt/mytv-work/statut/<codeur>.contrat.demande`.
- Interdit : contacter le vrai fournisseur ou tout hote de production, lire des secrets, toucher a /opt/mytv-relais, /opt/mytv-flux, /opt/mytv-ui ou a la configuration systeme.
