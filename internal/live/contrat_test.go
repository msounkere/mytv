package live

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// US-30 CA3 : le contrat docs/openapi.yaml decrit la playlist du direct telle que le code la
// sert : parametres (forcer, cap, X-Mytv-Capacites), modes, codes d'erreur.
func TestContratDeLaPlaylistDuDirect(t *testing.T) {
	brut, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Paths map[string]struct {
			Get struct {
				Description string `yaml:"description"`
				Parameters  []struct {
					Name   string `yaml:"name"`
					In     string `yaml:"in"`
					Schema struct {
						Enum []string `yaml:"enum"`
					} `yaml:"schema"`
				} `yaml:"parameters"`
				Responses map[string]struct {
					Headers map[string]struct {
						Schema struct {
							Enum []string `yaml:"enum"`
						} `yaml:"schema"`
					} `yaml:"headers"`
				} `yaml:"responses"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(brut, &c); err != nil {
		t.Fatal(err)
	}
	op := c.Paths["/live/{fichier}"].Get
	trouve := map[string]string{}
	for _, p := range op.Parameters {
		trouve[p.Name] = p.In
		if p.Name == "forcer" && strings.Join(p.Schema.Enum, ",") != "audio,video" {
			t.Errorf("forcer : enum = %v (jamais direct ni refus)", p.Schema.Enum)
		}
	}
	// US-38 CA1 : `qualite` (query) et `X-Mytv-Qualite` (header), valeurs auto|eco.
	for nom, ou := range map[string]string{"forcer": "query", "cap": "query", "X-Mytv-Capacites": "header", "qualite": "query", "X-Mytv-Qualite": "header"} {
		if trouve[nom] != ou {
			t.Errorf("parametre %s (%s) absent du contrat", nom, ou)
		}
	}
	for _, p := range op.Parameters {
		if (p.Name == "qualite" || p.Name == "X-Mytv-Qualite") && strings.Join(p.Schema.Enum, ",") != string(QualiteAuto)+","+string(QualiteEco) {
			t.Errorf("%s : enum = %v", p.Name, p.Schema.Enum)
		}
	}
	if got := strings.Join(op.Responses["200"].Headers["X-Mytv-Qualite"].Schema.Enum, ","); got != "eco" {
		t.Errorf("en-tete de reponse X-Mytv-Qualite : %q", got)
	}
	for _, code := range []string{"200", "400", "401", "415", "502", "503"} {
		if _, ok := op.Responses[code]; !ok {
			t.Errorf("reponse %s absente du contrat", code)
		}
	}
	for h, modes := range map[string]string{"X-Mytv-Mode": "direct,audio,video"} {
		if got := strings.Join(op.Responses["200"].Headers[h].Schema.Enum, ","); got != modes {
			t.Errorf("en-tete %s : %q, attendu %q", h, got, modes)
		}
	}
	for _, h := range []string{"X-Mytv-Codec", "X-Mytv-Hauteur"} {
		if _, ok := op.Responses["200"].Headers[h]; !ok {
			t.Errorf("en-tete %s absent du contrat", h)
		}
	}
	for _, mot := range []string{"format_hevc_non_lisible", "transcodage_impossible", "forcer_invalide", "qualite_invalide", "Absence = navigateur suppose"} {
		if !strings.Contains(op.Description+string(brut), mot) {
			t.Errorf("%q non documente", mot)
		}
	}
	// Les modes du code sont ceux du contrat.
	for _, m := range []Mode{ModeDirect, ModeAudio, ModeVideo, ModeRefus} {
		if !strings.Contains(op.Description, "`"+string(m)+"`") {
			t.Errorf("mode %s non decrit", m)
		}
	}
}
