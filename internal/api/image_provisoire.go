package api

// PROVISOIRE (US-28) : (*API).image est fourni par c4-a (internal/api/image.go, proxy
// d'images US-23). Ce fichier n'existe que pour que cette branche compile avant la
// fusion de c4-a ; il est SUPPRIME au rebase sur main apres la fusion de c4-a.
func (a *API) image(u string) string { return u }
