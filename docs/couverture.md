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
| US-01 | CA1 | internal/stockage/stockage_test.go::TestMigrationsRejouablesSansErreur ; ::TestBaseTropRecenteRefusee | OK |
| US-01 | CA2 | internal/stockage/stockage_test.go::TestEchecEnCoursDEcritureConserveAncienCatalogue ; ::TestDoublonDansInstantaneAnnuleEcriture ; ::TestContexteAnnuleAnnuleEcriture | OK |
| US-01 | CA3 | internal/stockage/stockage_test.go::TestCataloguesDeDeuxFournisseursCoexistent ; ::TestElementDUnAutreFournisseurRefuse | OK |
| US-01 | CA4 | internal/stockage/stockage_test.go::TestChainesFiltreRechercheEtPagination ; ::TestRechercheNeTrouvePasUneChaineRemplacee | OK |
| US-01 | CA5 | internal/stockage/stockage_test.go::TestCategoriesOrdonneesParType ; ::TestChaineAbsenteRenvoieErrNonTrouve | OK |
| US-01 | CA6 | internal/stockage/stockage_test.go::TestDerniereSynchroZeroSiJamaisEcrit ; ::TestDerniereSynchroConserveLInstant | OK |
| US-01 | CA7 | internal/stockage/stockage_test.go::TestVolumeReelEcritureEtRecherche (seuils hors -race) ; Benchmark* ; mesures dans le commit (1,4 s / 3-4 ms) | OK |
| US-01 | CA8 | internal/stockage/stockage_test.go::TestLecturesPendantEcritureSansVerrou | OK |
| US-06 | CA1 | internal/hls/hls_test.go::TestReecritureResoutEtSigne ; ::TestJetonFalsifieRefuse ; ::TestJetonNeRevelePasLURL ; ::TestReecritureAttributURI | OK |
| US-06 | CA2 | internal/hls/hls_test.go::TestUrlsRelativesALaRacine | OK |
| US-06 | CA3 | internal/hls/hls_test.go::TestUrlsDeSegmentsStablesMalgreLeHashDuFournisseur ; ::TestPorteesIsolees ; ::TestTableBorneeEtPurgee ; ::TestJetonDeterministe | OK |
| US-06 | CA4 | internal/hls/hls_test.go::TestPlaylistMaitresseSousPlaylistsReecrites ; ::TestPlaylistMaitresseAttributsMediaReecritsEnSousPlaylists | OK |
| US-06 | CA5 | internal/hls/hls_test.go::TestAucuneUrlExterneLisibleSansSignature ; ::TestJetonNeRevelePasLURL | OK |
| US-02 | CA1 | internal/catalogue/synchro_test.go::TestSynchroniserTelechargePuisEcrit ; ::TestEchecFournisseurConserveAncienCatalogue ; ::TestEchecEcritureRenvoyeeEtJournalisee ; internal/catalogue/integration_test.go::TestIntegrationXtreamFauxFournisseurEtSQLite | OK |
| US-02 | CA2 | internal/catalogue/synchro_test.go::TestSiPerimeNeTelechargePasUnCatalogueFrais ; ::TestSiPerimeApresEchecRetelecharge | OK |
| US-02 | CA3 | internal/catalogue/synchro_test.go::TestPlanificationQuotidienneALHeureLocale ; ::TestSynchroAuDemarrage ; ::TestPlanificationRetenteApresEchec ; ::TestPlanifierRendLaMainALAnnulation | OK |
| US-02 | CA4 | internal/catalogue/synchro_test.go::TestDemandesSimultaneesPartagentUnSeulTelechargement ; ::TestAnnulationDUnDemandeurEtDuDernier ; ::TestFournisseursSynchroniseSansSeBloquer | OK |
| US-02 | CA5 | internal/catalogue/synchro_test.go::TestFournisseurInactifJamaisSynchronise ; ::TestUnEchecNEmpechePasLesAutres ; ::TestPlanificationUnEchecNArretePasLesAutres | OK |
| US-02 | CA6 | internal/catalogue/synchro_test.go::TestJournalDureeEtVolumesSansIdentifiants ; internal/catalogue/integration_test.go::(mot de passe absent des journaux) | OK |
| US-07 | CA1 | internal/live/relais_test.go::TestPlaylistNouvelEssaiApresEchec ; ::TestPlaylistNombreDEssaisVientDeLaConfiguration ; ::TestPlaylistDemandesSimultaneesRegroupees ; ::TestPlaylistFraicheurReutilisee ; ::TestPlaylistRefusNetPasDeNouvelEssai | OK |
| US-07 | CA2 | internal/live/relais_test.go::TestPlaylistDeSecoursQuandLAmontTombe ; ::TestPlaylistSansSecoursOuPerimee502 ; internal/live/integration_test.go::TestSecoursAvecLeFauxFournisseur | OK |
| US-07 | CA3 | internal/live/relais_test.go::TestSegmentCopieEtMisEnCache ; ::TestCacheBorneEnNombre ; ::TestCacheBorneEnOctets ; ::TestSegmentTropGrosRelayeSansEtreGarde ; ::TestSegmentRangeTransmisEtNonCache ; ::TestSegmentDeuxEssais ; internal/live/integration_test.go::TestSegmentCoupeNEstPasMisEnCache | OK |
| US-07 | CA4 | internal/live/relais_test.go::TestRequetesAmontPassentParUnBailRenouvele ; ::TestSansPlaceAucuneRequeteAmontEt503 ; ::TestClientPartiLibereLeBail ; ::TestAnnulationDeLAncienneChaineApresZappingNeLiberePasLeBail ; internal/live/integration_test.go::TestFinDeLectureLibereLaPlaceDansLesVraisBaux | OK |
| US-07 | CA5 | internal/live/relais_test.go::TestPlaylistPatienteSur407 ; ::TestPlaylistPatienceEpuisee503 ; ::TestSegmentPatienteSur407 ; internal/live/integration_test.go::TestZappingAvecLeFauxFournisseurUneSeuleConnexion ; internal/live/integration_test.go::TestZappingPatienceEpuiseeRend503 | OK |
| US-07 | CA6 | internal/live/relais_test.go::TestEtatExposeLesStatistiques ; ::TestCentile | OK |
| US-07 | CA7 | internal/live/relais_test.go::TestRefusParDefaut | OK |
| US-08 | CA1 | internal/comptes/comptes_test.go::TestMigrationsVersionneesEtRejouables ; ::TestMotDePasseJamaisEnClairEtParametresDeLaConfig ; ::TestEmpreinteAncienneRenouvelee ; ::TestNomsEtMotsDePasseRefuses ; ::TestMotDePasseGeantRefuse ; ::TestNomAvecSQLNeCasseRien | OK |
| US-08 | CA2 | comptes_test.go::TestErreurIdentiquePourInconnuEtMauvaisMotDePasse ; ::TestTempsDeReponseComparablePourCompteInconnu ; ::TestBlocageParCompte ; ::TestBlocageParAdresse ; ::TestBlocageIPv6ParPrefixe ; ::TestDelaiCroissantPuisOubli ; ::TestRafaleParalleleBornee ; ::TestSuccesRemetLeCompteAZero ; ::TestConnexionHTTP429 | OK |
| US-08 | CA3 | comptes_test.go::TestSessionJetonHacheEtExpiration ; ::TestAttributsDuCookie ; ::TestDeconnexionImmediateEtTerminer | OK |
| US-08 | CA4 | comptes_test.go::TestCompteLocalLimiteAuxPlages ; ::TestCompteLocalDesactiveParDefaut ; ::TestPlagesInvalidesRefusees | OK |
| US-08 | CA5 | comptes_test.go::TestAuthentifierRefusParDefaut ; ::TestAdminEtIdentifiantDeSession ; internal/comptes/live_test.go::TestAuthentificateurBrancheSurLeRelais | OK |
| US-08 | CA6 | comptes_test.go::TestProfils ; ::TestChangementDeMotDePasseRevoqueLesAutresSessions ; ::TestConcurrence | OK |
| US-08 | CA7 | comptes_test.go::TestAucunSecretDansErreursEtJournaux | OK |
| US-08 | CA8 | comptes_test.go::TestDeconnexionImmediateEtTerminer (doublure de Terminateur). Cote baux : `baux.Terminer` pas encore implemente (US-10 CA10) | OK (cablage a faire) |

## Couverture de lignes (fin du lot 1)
baux 93,6 % · hls 95,6 % · live 92,3 % · config 91,5 % · fauxiptv 89,7 % · xtream 88,7 % · stockage 83,8 %. Aucun paquet sous 70 % ; catalogue 91,0 %.

## Ecarts
Aucun CA du lot 1 fusionne sans test. Reserves :
- US-03 CA4 : seul `reseau.delai_entete` est exerce par un test ; `delai_connexion` n'a pas de test dedie.
- US-04 CA3 : les pannes 458 et 405 ne sont verifiees que via les tests du client Xtream (pas de test propre au faux).
