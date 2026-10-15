package remux

import "mytv/internal/config"

// DepuisConfig derive les reglages du lanceur, du sondeur et de l'executeur de
// la section `remux` : aucun chiffre n'est decide ici.
func DepuisConfig(c config.Remux) (Reglages, ReglagesSonde, ExecOS) {
	return Reglages{Ffmpeg: c.Ffmpeg, ProcessusMax: c.ProcessusMax, FileMax: c.FileMax, AttenteFile: c.AttenteFile.Std()},
		ReglagesSonde{Ffprobe: c.Ffprobe, Duree: c.DureeSonde.Std(), Delai: c.DelaiSonde.Std(), Max: c.SondeMax},
		ExecOS{DelaiArret: c.DelaiArret.Std()}
}
