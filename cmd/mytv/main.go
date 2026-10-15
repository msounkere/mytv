// Commande mytv : le serveur.
//
//	mytv --config mytv.yaml            lance le service
//	mytv --config mytv.yaml --verifier-config
//	                                   controle la configuration et les secrets, sans rien ouvrir
//	mytv --config mytv.yaml --sauvegarder copie.db
//	                                   ecrit une sauvegarde coherente et verifiee de la base (le service peut tourner)
//
// SIGINT et SIGTERM declenchent un arret propre (requetes en cours terminees dans
// serveur.delai_arret, taches planifiees attendues).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"mytv/internal/appli"
	"mytv/internal/config"
	"mytv/internal/sauvegarde"
)

func main() {
	ctx, arreter := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer arreter()
	os.Exit(executer(ctx, os.Args[1:], os.Stderr))
}

func executer(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mytv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	chemin := fs.String("config", "mytv.yaml", "fichier de configuration")
	verifier := fs.Bool("verifier-config", false, "controle la configuration et les secrets puis quitte")
	copie := fs.String("sauvegarder", "", "ecrit une sauvegarde coherente de la base dans ce fichier (qui ne doit pas exister) puis quitte")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *copie != "" && *verifier {
		fmt.Fprintln(stderr, "--sauvegarder et --verifier-config s'excluent")
		return 2
	}
	cfg, err := config.Charger(*chemin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *copie != "" {
		return sauvegarder(ctx, cfg, *copie, stderr)
	}
	if *verifier {
		if err := appli.Verifier(cfg); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, w := range cfg.Avertissements() {
			fmt.Fprintln(stderr, "avertissement :", w)
		}
		fmt.Fprintln(stderr, "configuration valide")
		return 0
	}
	a, err := appli.Construire(ctx, cfg, appli.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "demarrage impossible :", err)
		return 1
	}
	if err := a.Lancer(ctx); err != nil {
		fmt.Fprintln(stderr, "arret sur erreur :", err)
		return 1
	}
	return 0
}

// sauvegarder ecrit une copie coherente de stockage.fichier dans `destination`. La base
// est ouverte en lecture seule : aucune migration, aucun verrou d'ecriture, le service
// peut tourner. Aucun secret n'est lu : seul le chemin de la base compte.
func sauvegarder(ctx context.Context, cfg *config.Config, destination string, stderr io.Writer) int {
	db, err := sauvegarde.OuvrirLectureSeule(cfg.Stockage.Fichier)
	if err != nil {
		fmt.Fprintln(stderr, "sauvegarde impossible :", err)
		return 1
	}
	defer db.Close()
	octets, err := sauvegarde.Copier(ctx, db, destination)
	if err != nil {
		fmt.Fprintln(stderr, "sauvegarde impossible :", err)
		return 1
	}
	fmt.Fprintf(stderr, "sauvegarde ecrite : %s (%d octets, integrity_check ok)\n", destination, octets)
	return 0
}
