// Commande mytv-comptes : administration locale des comptes (US-08).
//
//	mytv-comptes creer --config mytv.yaml --nom alice --admin < mot_de_passe.txt
//	mytv-comptes creer --config mytv.yaml --nom alice --mot-de-passe-fichier /chemin
//
// Le mot de passe n'est JAMAIS un argument de ligne de commande (il resterait dans
// `ps` et dans l'historique du shell) : il vient de l'entree standard ou d'un fichier.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"mytv/internal/comptes"
	"mytv/internal/config"
)

func main() {
	os.Exit(executer(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func executer(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "creer" {
		fmt.Fprintln(stderr, "usage : mytv-comptes creer --config FICHIER --nom NOM [--admin] [--mot-de-passe-fichier FICHIER]")
		return 2
	}
	fs := flag.NewFlagSet("creer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := fs.String("config", "mytv.yaml", "fichier de configuration")
	nom := fs.String("nom", "", "nom de l'utilisateur")
	admin := fs.Bool("admin", false, "administrateur")
	fichier := fs.String("mot-de-passe-fichier", "", "fichier contenant le mot de passe (defaut : entree standard)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *nom == "" {
		fmt.Fprintln(stderr, "--nom est requis")
		return 2
	}
	entree := stdin
	if *fichier != "" {
		f, err := os.Open(*fichier)
		if err != nil {
			fmt.Fprintln(stderr, "mot de passe : fichier illisible")
			return 1
		}
		defer f.Close()
		entree = f
	}
	c, err := config.Charger(*cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	db, err := comptes.OuvrirBase(c.Stockage.Fichier)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	svc, err := comptes.Nouveau(ctx, comptes.Options{DB: db, Config: c.Comptes})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	u, err := svc.CreerDepuisEntree(ctx, *nom, *admin, entree)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "utilisateur %q cree (admin : %t)\n", u.Nom, u.Admin)
	return 0
}
