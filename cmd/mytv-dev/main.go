// Commande mytv-dev : faux fournisseur IPTV + mytv, pour developper et tester
// l'interface sans toucher au vrai fournisseur. N'ecoute que sur 127.0.0.1.
//
//	mytv-dev --dist /opt/mytv-ui/dist --ecoute 127.0.0.1:8080
//	mytv-dev --panne 407:30s --latence 800ms
//
// Arret propre sur SIGINT/SIGTERM. Voir internal/dev.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mytv/internal/dev"
)

func main() {
	ctx, arreter := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer arreter()
	os.Exit(executer(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

type liste []string

func (l *liste) String() string     { return strings.Join(*l, ",") }
func (l *liste) Set(v string) error { *l = append(*l, v); return nil }

func executer(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mytv-dev", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o dev.Options
	var pannes liste
	fs.StringVar(&o.Ecoute, "ecoute", "", "adresse de mytv (defaut 127.0.0.1:0)")
	fs.StringVar(&o.Fournisseur, "fournisseur", "", "adresse du faux fournisseur (defaut 127.0.0.1:0)")
	fs.StringVar(&o.Controle, "controle", "", "adresse de l'API de pilotage des pannes (defaut 127.0.0.1:0)")
	fs.StringVar(&o.Dist, "dist", "", "dossier de l'interface a servir (meme origine que l'API)")
	fs.IntVar(&o.Chaines, "chaines", 0, "nombre de chaines (defaut 2000)")
	fs.IntVar(&o.Films, "films", 0, "nombre de films (defaut 5000)")
	fs.IntVar(&o.Series, "series", 0, "nombre de series (defaut 500)")
	fs.Var(&pannes, "panne", "panne de depart « statut:duree », ex. 407:30s (repetable)")
	fs.DurationVar(&o.Latence, "latence", 0, "latence ajoutee aux reponses du direct et des films, ex. 800ms")
	fs.DurationVar(&o.UneConnexion, "une-connexion", 0, "regle « une chaine a la fois » du fournisseur, ex. 100s")
	fs.StringVar(&o.Utilisateur, "utilisateur", "", "utilisateur de test (defaut test)")
	fs.StringVar(&o.MotDePasse, "mot-de-passe", "", "mot de passe des comptes de test (defaut motdepasse-de-dev)")
	fs.BoolVar(&o.Verbeux, "verbeux", false, "journal detaille sur la sortie d'erreur")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	o.Pannes = pannes
	o.Journal = stderr

	debut := time.Now()
	h, err := dev.Demarrer(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "demarrage impossible :", err)
		return 1
	}
	defer h.Fermer()
	fmt.Fprintf(stdout, "mytv-dev pret en %s\n", time.Since(debut).Round(10*time.Millisecond))
	fmt.Fprintf(stdout, "  mytv (API + interface) : %s\n", h.URL)
	fmt.Fprintf(stdout, "  faux fournisseur       : %s\n", h.FournisseurURL)
	fmt.Fprintf(stdout, "  pilotage des pannes    : %s  (POST /panne?statut=407&duree=30s, DELETE /panne, GET /compteurs)\n", h.ControleURL)
	fmt.Fprintf(stdout, "  utilisateur de test    : %s / %s   (administrateur : %s)\n", h.Utilisateur, h.MotDePasse, h.Admin)
	if o.Dist == "" {
		fmt.Fprintln(stdout, "  interface              : non servie (--dist dossier)")
	}
	if err := h.Attendre(); err != nil {
		fmt.Fprintln(stderr, "arret sur erreur :", err)
		return 1
	}
	return 0
}
