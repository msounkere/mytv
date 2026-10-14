# Matrice de couverture (tenue par le coordinateur)

Pour chaque critere d'acceptation de docs/backlog.md : le test automatique qui l'exerce. Un CA sans test est un ECART.

| US | CA | Test (fichier::fonction) | Statut |
|----|----|--------------------------|--------|
| US-03 | CA1 | internal/xtream/xtream_test.go::TestCatalogueProduitUnInstantaneAvecContenuID | OK |
| US-03 | CA2 | xtream_test.go::TestCatalogueToleranteAuxJSONVaries ; ::TestCatalogueRefuseUneReponseQuiNEstPasUneListe | OK |
| US-03 | CA3 | xtream_test.go::TestStatutsTraduitsEnErreursTypees ; ::TestCatalogueTraduitLes458 ; ::TestMauvaisMotDePasseDonneErrIdentifiants ; ::TestDelaiEnteteDepasseDonneErrAmont | OK |
| US-03 | CA4 | xtream_test.go::TestRequeteSuitLesRedirections ; ::TestRequeteLimiteLesRedirectionsADix ; ::TestRequetePoseLesEntetesParDefaut (UA, Accept-Encoding gzip/identity) ; ::TestRequeteNeLitPasLeCorpsEnEntier. Delais reseau.* : seul delai_entete est exerce | OK (delais partiels) |
| US-03 | CA5 | xtream_test.go::TestAucunIdentifiantDansErreursNiJournaux | OK |
| US-03 | CA6 | xtream_test.go::TestURLLiveEchappeLesIdentifiants ; ::TestURLLiveEstAccepteeParLeFournisseur | OK |
| US-04 | CA1 | internal/testutil/fauxiptv/fauxiptv_test.go::TestAPIServLeCatalogueConfigure ; ::TestPlaylistAFenetreGlissante ; ::TestSegmentALaTailleConfiguree | OK |
| US-04 | CA2 | fauxiptv_test.go::TestHashDeCheminChangeAChaquePlaylist ; ::TestSegmentServiAvecUnAncienHash | OK |
| US-04 | CA3 | fauxiptv_test.go::TestPanneStatutPourNDemandes ; ::TestPanneLimiteeDansLeTemps ; ::TestLatenceRespecteLAnnulationDuClient ; ::TestCoupureEnMilieuDeSegment ; ::TestCorpsVide ; ::TestPanneVisantLesSegmentsEpargneLesPlaylists (458/502/405 via le champ Statut, 458 et 405 couverts par xtream_test) | OK |
| US-04 | CA4 | fauxiptv_test.go::TestUneSeuleConnexionRefuseUneAutreChaine | OK |
| US-04 | CA5 | fauxiptv_test.go::TestCompteursEtTraces ; ::TestAccesConcurrents | OK |
| US-05 | CA1 | internal/baux/baux_test.go::TestAuPlusConnexionsMaxBauxSimultanes | OK |
| US-05 | CA2 | baux_test.go::TestZappingReutiliseLeBailSansCompterDeuxPlaces ; ::TestZappingRenouvelleLExpiration | OK |
| US-05 | CA3 | baux_test.go::TestBailNonRenouveleExpireApresLeTTL ; ::TestRenouvelerUnBailExpireNeLeRessuscitePas ; ::TestLibererEstImmediatEtIdempotent | OK |
| US-05 | CA4 | baux_test.go::TestAttenteObtientLaPlaceExpireeAuReessaiSuivant ; ::TestAnnulationDuContexteInterrompUneAttente ; ::TestSansPatienceLeRefusEstImmediat ; ::TestAuPlusConnexionsMaxBauxSimultanes (ErrPlaceIndisponible) | OK |
| US-05 | CA5 | baux_test.go::TestConcurrenceNeDepasseJamaisLeMaximum (race + fuite) ; horloge manuelle partout | OK |
| US-05 | CA6 | baux_test.go::TestFournisseursIndependants | OK |

## Couverture de lignes (apres US-05)
baux 93,6 % · config 91,2 % · fauxiptv 89,7 % · xtream 88,7 %. Aucun paquet sous 70 %.
