package epg

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mytv/internal/domain"
)

// ErrTropGros : le flux decompresse depasse la limite configuree (epg.taille_max).
var ErrTropGros = errors.New("epg : guide XMLTV trop volumineux")

// Canal est une <channel> du guide : son identifiant et ses noms d'affichage.
type Canal struct {
	ID   string
	Noms []string
	// Logo : <icon src> du canal (http/https seulement), vide si absent. Sert de repli
	// pour les chaines sans logo du fournisseur (US-29).
	Logo string
}

// ParamsAnalyse regle l'analyse d'un guide.
type ParamsAnalyse struct {
	// De, A : seuls les programmes qui chevauchent [De, A[ sont emis.
	De, A time.Time
	// Lieu : fuseau des dates sans decalage (« 20240101200000 »). Defaut UTC.
	Lieu *time.Location
	// TailleMax : octets decompresses lus au plus ; <= 0 = sans limite.
	TailleMax int64
}

// Stats compte ce que l'analyse a rencontre ou ecarte, pour le journal.
type Stats struct {
	Canaux, Programmes, Emis int
	HorsFenetre, Invalides   int
}

// maxTitre / maxDescription bornent le texte retenu : un guide hostile ou
// defectueux ne doit pas gonfler la memoire ni la base.
const (
	maxTitre       = 300
	maxDescription = 2000
	maxCategorie   = 100
	maxSousTitre   = 300
	maxEpisode     = 30   // « S02E05 » ou un libelle d'ecran court ; au-dela, ce n'est plus un numero
	maxLogo        = 500  // une URL d'icone raisonnable
	maxNumero      = 9999 // saison ou episode plausible
)

// Analyser lit un guide XMLTV EN FLUX (un element a la fois, jamais le document
// entier), gunzip automatique si le contenu commence par la signature gzip.
//
// Tolerance : champs absents, dates avec ou sans decalage/secondes, elements
// inconnus, XML un peu sale (mode non strict), encodage UTF-8 ou ISO-8859-1.
// Un programme illisible est compte dans Stats.Invalides et ignore, jamais fatal.
// Le Debut/Fin emis sont en UTC ; Fin est zero si le guide ne la donne pas
// (l'appelant la deduit du programme suivant).
func Analyser(ctx context.Context, r io.Reader, p ParamsAnalyse, surCanal func(Canal), surProgramme func(domain.Programme)) (Stats, error) {
	var st Stats
	lieu := p.Lieu
	if lieu == nil {
		lieu = time.UTC
	}
	src := io.Reader(&lecteurCtx{ctx: ctx, r: r})
	br := bufio.NewReaderSize(src, 64<<10)
	if sig, err := br.Peek(2); err == nil && sig[0] == 0x1f && sig[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return st, fmt.Errorf("epg : gzip : %w", err)
		}
		defer gz.Close()
		src = gz
	} else {
		src = br
	}
	if p.TailleMax > 0 {
		src = &limite{r: src, reste: p.TailleMax}
	}

	d := xml.NewDecoder(src)
	d.Strict = false
	d.CharsetReader = lecteurCharset
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return st, nil
		}
		if err != nil {
			if e := ctx.Err(); e != nil {
				return st, e
			}
			if errors.Is(err, ErrTropGros) {
				return st, err
			}
			return st, fmt.Errorf("epg : XMLTV illisible : %w", err)
		}
		deb, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch deb.Name.Local {
		case "channel":
			var c xmlCanal
			if err := d.DecodeElement(&c, &deb); err != nil {
				return st, erreurFlux(ctx, err)
			}
			st.Canaux++
			if id := strings.TrimSpace(c.ID); id != "" && surCanal != nil {
				cn := Canal{ID: id}
				for _, n := range c.Noms {
					if n = strings.TrimSpace(n); n != "" {
						cn.Noms = append(cn.Noms, tronquer(n, maxTitre))
					}
				}
				cn.Logo = c.logo()
				surCanal(cn)
			}
		case "programme":
			var x xmlProgramme
			if err := d.DecodeElement(&x, &deb); err != nil {
				return st, erreurFlux(ctx, err)
			}
			st.Programmes++
			prog, ok := x.convertir(lieu)
			if !ok {
				st.Invalides++
				continue
			}
			if !chevauche(prog, p.De, p.A) {
				st.HorsFenetre++
				continue
			}
			st.Emis++
			surProgramme(prog)
		}
	}
}

func erreurFlux(ctx context.Context, err error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if errors.Is(err, ErrTropGros) {
		return err
	}
	return fmt.Errorf("epg : XMLTV illisible : %w", err)
}

// chevauche : le programme intersecte [de, a[. Sans fin connue, seul le debut compte.
func chevauche(p domain.Programme, de, a time.Time) bool {
	if !a.IsZero() && !p.Debut.Before(a) {
		return false
	}
	if p.Fin.IsZero() {
		return de.IsZero() || !p.Debut.Before(de)
	}
	return de.IsZero() || p.Fin.After(de)
}

type xmlCanal struct {
	ID    string   `xml:"id,attr"`
	Noms  []string `xml:"display-name"`
	Icons []struct {
		Src string `xml:"src,attr"`
	} `xml:"icon"`
}

// logo : la premiere icone utilisable. Seules les adresses http(s) sont retenues : le
// guide est une donnee du fournisseur, et l'adresse sera relayee par le proxy d'images
// (qui la controle de toute facon) ; on n'enregistre pas ce qui ne pourrait jamais
// etre servi (data:, file:, chemin relatif).
func (c xmlCanal) logo() string {
	for _, i := range c.Icons {
		u := strings.TrimSpace(i.Src)
		l := strings.ToLower(u)
		if len(u) <= maxLogo && (strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")) {
			return u
		}
	}
	return ""
}

type xmlProgramme struct {
	Debut      string   `xml:"start,attr"`
	Fin        string   `xml:"stop,attr"`
	Canal      string   `xml:"channel,attr"`
	Titres     []string `xml:"title"`
	Descs      []string `xml:"desc"`
	Categories []string `xml:"category"`
	SousTitres []string `xml:"sub-title"`
	Dates      []string `xml:"date"`
	Episodes   []struct {
		Systeme string `xml:"system,attr"`
		Texte   string `xml:",chardata"`
	} `xml:"episode-num"`
}

func (x xmlProgramme) convertir(lieu *time.Location) (domain.Programme, bool) {
	id := strings.TrimSpace(x.Canal)
	debut, ok := ParserDate(x.Debut, lieu)
	if id == "" || !ok {
		return domain.Programme{}, false
	}
	p := domain.Programme{EpgID: id, Debut: debut}
	if fin, ok := ParserDate(x.Fin, lieu); ok {
		if !fin.After(debut) {
			return domain.Programme{}, false // duree nulle ou negative : donnee corrompue
		}
		p.Fin = fin
	}
	p.Titre = tronquer(premierNonVide(x.Titres), maxTitre)
	p.Description = tronquer(premierNonVide(x.Descs), maxDescription)
	p.Categorie = tronquer(premierNonVide(x.Categories), maxCategorie)
	p.SousTitre = tronquer(premierNonVide(x.SousTitres), maxSousTitre)
	p.Annee = annee(premierNonVide(x.Dates))
	p.Episode = x.episode()
	return p, true
}

// annee : <date> vaut « 2019 » ou « 20190315 » ; on garde l'annee si elle est plausible.
func annee(s string) string {
	if len(s) < 4 {
		return ""
	}
	for _, r := range s[:4] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	if a, _ := strconv.Atoi(s[:4]); a < 1880 || a > 2100 {
		return ""
	}
	return s[:4]
}

// episode donne un numero lisible. Un libelle « onscreen » (« S02E05 ») est garde tel quel
// s'il est court et porte un chiffre ; sinon le systeme « xmltv_ns » (« 1.4. » : saison et
// episode, comptes depuis ZERO) est converti en « S02E05 ». Les autres systemes
// (identifiants de programme, imdb...) ne sont pas un numero a afficher.
func (x xmlProgramme) episode() string {
	var ns string
	for _, e := range x.Episodes {
		t := strings.Join(strings.Fields(e.Texte), " ")
		switch strings.ToLower(strings.TrimSpace(e.Systeme)) {
		case "onscreen":
			if t != "" && len(t) <= maxEpisode && strings.ContainsAny(t, "0123456789") {
				return t
			}
		case "xmltv_ns":
			if ns == "" {
				ns = t
			}
		}
	}
	return depuisXMLTVNS(ns)
}

func depuisXMLTVNS(s string) string {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return ""
	}
	num := func(p string) (int, bool) {
		p, _, _ = strings.Cut(p, "/") // « 4/12 » : episode 4 sur 12
		p = strings.TrimSpace(p)
		if p == "" {
			return 0, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n+1 > maxNumero {
			return 0, false
		}
		return n + 1, true
	}
	sa, okS := num(parts[0])
	ep, okE := num(parts[1])
	switch {
	case okS && okE:
		return fmt.Sprintf("S%02dE%02d", sa, ep)
	case okE:
		return fmt.Sprintf("E%02d", ep)
	}
	return ""
}

func premierNonVide(l []string) string {
	for _, s := range l {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func tronquer(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) { // ne coupe pas au milieu d'une rune
		s = s[:len(s)-1]
	}
	return s
}

// ParserDate lit une date XMLTV : AAAAMMJJ[HH[MM[SS]]] suivie d'un decalage facultatif
// (« +0100 », « -0530 », « +01:00 », « Z », « UTC »), avec ou sans espace. Sans decalage,
// la date est dans `lieu`. Le resultat est en UTC.
func ParserDate(s string, lieu *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	chiffres, reste := s[:i], strings.TrimSpace(s[i:])
	var fmtDate string
	switch len(chiffres) {
	case 8:
		fmtDate = "20060102"
	case 10:
		fmtDate = "2006010215"
	case 12:
		fmtDate = "200601021504"
	case 14:
		fmtDate = "20060102150405"
	default:
		return time.Time{}, false
	}
	loc := lieu
	if loc == nil {
		loc = time.UTC
	}
	if reste != "" {
		dec, ok := decalage(reste)
		if !ok {
			return time.Time{}, false
		}
		loc = dec
	}
	t, err := time.ParseInLocation(fmtDate, chiffres, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func decalage(s string) (*time.Location, bool) {
	switch strings.ToUpper(s) {
	case "Z", "UTC", "GMT":
		return time.UTC, true
	}
	if len(s) < 3 || (s[0] != '+' && s[0] != '-') {
		return nil, false
	}
	signe := 1
	if s[0] == '-' {
		signe = -1
	}
	v := strings.ReplaceAll(s[1:], ":", "")
	var h, m int
	switch len(v) {
	case 2:
		if _, err := fmt.Sscanf(v, "%2d", &h); err != nil {
			return nil, false
		}
	case 4:
		if _, err := fmt.Sscanf(v, "%2d%2d", &h, &m); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	if h > 14 || m > 59 {
		return nil, false
	}
	return time.FixedZone("", signe*(h*3600+m*60)), true
}

// lecteurCtx fait echouer la lecture des que ctx est annule : l'analyse est
// interrompue meme si le fournisseur n'envoie plus rien.
type lecteurCtx struct {
	ctx context.Context
	r   io.Reader
}

func (l *lecteurCtx) Read(p []byte) (int, error) {
	if err := l.ctx.Err(); err != nil {
		return 0, err
	}
	return l.r.Read(p)
}

// limite refuse de lire plus de `reste` octets (contrairement a io.LimitReader,
// qui ferait croire a une fin de fichier normale).
type limite struct {
	r     io.Reader
	reste int64
}

func (l *limite) Read(p []byte) (int, error) {
	if l.reste <= 0 {
		return 0, ErrTropGros
	}
	if int64(len(p)) > l.reste {
		p = p[:l.reste]
	}
	n, err := l.r.Read(p)
	l.reste -= int64(n)
	return n, err
}

// lecteurCharset gere les encodages vus dans les guides XMLTV sans dependance :
// UTF-8 (deja natif) et ISO-8859-1 / Windows-1252 (traites comme Latin-1).
func lecteurCharset(charset string, entree io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return entree, nil
	case "iso-8859-1", "latin1", "iso8859-1", "windows-1252", "cp1252":
		return &latin1{r: bufio.NewReader(entree)}, nil
	}
	return nil, fmt.Errorf("encodage %q non pris en charge", charset)
}

type latin1 struct {
	r   *bufio.Reader
	tmp []byte
}

func (l *latin1) Read(p []byte) (int, error) {
	n := 0
	for len(l.tmp) > 0 && n < len(p) {
		p[n] = l.tmp[0]
		l.tmp = l.tmp[1:]
		n++
	}
	if len(p) < 2 && n == 0 && len(p) == 1 {
		b, err := l.r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b < 0x80 {
			p[0] = b
		} else {
			p[0], l.tmp = 0xC0|b>>6, []byte{0x80 | b&0x3F}
		}
		return 1, nil
	}
	for n < len(p)-1 { // 2 octets de marge pour une rune
		b, err := l.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b < 0x80 {
			p[n] = b
			n++
		} else {
			p[n] = 0xC0 | b>>6
			p[n+1] = 0x80 | b&0x3F
			n += 2
		}
		if l.r.Buffered() == 0 {
			break // ne bloque pas en attendant un flux lent
		}
	}
	return n, nil
}
