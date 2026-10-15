// Commande mytv : le serveur.
//
//	mytv --config mytv.yaml            lance le service
//	mytv --config mytv.yaml --verifier-config
//	                                   controle la configuration et les secrets, sans rien ouvrir
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Charger(*chemin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *verifier {
		if err := appli.Verifier(cfg); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
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
