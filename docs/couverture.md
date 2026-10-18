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
| US-09 | CA1 | internal/epg/xmltv_test.go::TestAnalyseGzip ; ::TestAnalyseEnFluxSansToutCharger ; ::TestAnalyseTailleMax ; service_test.go::TestRafraichirGuideGzip | OK |
| US-09 | CA2 | xmltv_test.go::TestParserDateToutesLesFormes ; ::TestAnalyseDuJeuDEssai ; ::TestAnalyseFuseauParDefaut ; ::TestAnalyseLatin1 ; ::TestAnalyseTexteLongTronque | OK |
| US-09 | CA3 | rapprochement_test.go::* ; normalise_test.go ; service_test.go::TestRafraichirIngereLeGuide | OK |
| US-09 | CA4 | base_test.go::TestEcritureRemplaceParFournisseur ; ::TestEchecEnCoursDEcritureConserveAncienEPG ; service_test.go::TestEchecConserveLAncienEPG | OK |
| US-09 | CA5 | base_test.go::TestEnCoursEtSuivant ; ::TestGrilleParPlageHoraire ; ::TestGrilleBeaucoupDeChaines | OK |
| US-09 | CA6 | service_test.go::TestSiPerimeRespecteLAgeMax ; ::TestSingleflightParFournisseur ; ::TestPlanifierRespecteLaFrequence ; ::TestAnnulationDuDernierDemandeurInterromptLeVol | OK |
| US-09 | CA7 | internal/epg/bench_test.go::BenchmarkIngestion25000Chaines500000Programmes (12,0 s) | OK |
| US-09 | CA8 | internal/stockage/stockage_test.go (filtre FournisseurID) OK ; domain.StockageVideo (Films/Film/Series/Serie) NON LIVRE | ECART : reporte en US-13 CA8 |
| US-09 | CA8 (rattrape) | internal/stockage/stockage_test.go::TestStockageVideoFilmsEtSeries ; ::TestChainesFiltreParFournisseur | OK |
| US-13 | CA1 | internal/profil/profil_test.go::TestFavorisIdempotentsEtOrdreStable ; ::TestFavorisLimite | OK |
| US-13 | CA2 | profil_test.go::TestHistoriqueRepriseBorneEtPurge ; ::TestHistoriqueMemeInstantEtPositionsIncoherentes | OK |
| US-13 | CA3 | profil_test.go::TestIsolationEntreProfils | OK |
| US-13 | CA4 | profil_test.go::TestOrphelinsIgnoresALaLecture ; internal/profil/integration_test.go::TestOrphelinsAvecLeVraiStockage | OK |
| US-13 | CA5 | profil_test.go::TestEcrituresConcurrentes ; ::TestConflitEntreDeuxAppareils | OK |
| US-13 | CA6 | routes /profil/* : US-11 | EN ATTENTE (US-11) |
| US-13 | CA7 | profil_test.go::TestConflitEntreDeuxAppareils ; ::TestReglagesLimitesDeTaille | OK |
| US-10 | CA1 | internal/vod/banc_test.go::TestDirectLectureComplete ; ::TestDirectRange ; ::TestDirectRefuseUn200ALaPlaceD ; ::TestDirectRefuseUnContentRangeIncoherent ; ::TestDirectReprendApresUneCoupure ; ::TestDirectClientPartiFermeLAmont ; ::TestDirectHEAD | OK |
| US-10 | CA2 | internal/remux/remux_test.go::TestDeciderTable ; ::TestAnalyserSondeIgnoreLesPochettes ; ::TestSondeMiseEnCache ; ::TestSondeUneSeuleExecutionPourDesDemandesSimultanees ; ::TestSondeDelaiMaximal | OK |
| US-10 | CA3 | remux_test.go::TestLanceurRespecteProcessusMaxEtLaFile ; ::TestLanceurAttenteDepassee ; ::TestLanceurLeContexteAnnuleTueLeProcessus ; internal/remux/exec_test.go::TestExecOSArreterTueLeGroupe ; ::TestExecOSSigkillSiSigtermIgnore ; vod::TestRemuxMKV ; ::TestRemuxAudioConverti ; ::TestRemuxSaturation ; ::TestRemuxClientPartiArreteFfmpeg | OK |
| US-10 | CA4 | internal/vod/banc_test.go::TestBailPrisPuisRendu ; ::TestPasDePlace ; ::TestBailRenouvelePendantLaLecture ; ::TestZappingRemplaceLaLectureEnCours | OK |
| US-10 | CA5 | remux_test.go::TestArgumentsRefusentLesEntreesDouteuses ; ::TestLanceurRefuseUneTacheInvalideSansRienLancer ; exec_test.go::TestExecOSArgumentsSansShell ; vod::TestLeClientNInfluencePasLURLAmont ; ::TestIdentifiantsInconnusOuMalFormes | OK |
| US-10 | CA6 | exec_test.go::TestLanceurAvecFauxFfmpeg ; ::TestReelSondeEtRemux (ffmpeg reel, -short exclu) ; vod::TestIntegrationFauxFfmpegClientParti | OK |
| US-10 | CA7 | internal/xtream/xtream_test.go::TestSaisonsObjetParSaison ; ::TestSaisonsFormesBizarres ; ::TestSaisonsErreursTypees ; ::TestURLFilmEtEpisode ; vod::TestEpisodeParSonIdentifiant | OK |
| US-10 | CA8 | vod::TestStatutHTTP ; TestNouveauValide ; TestReglagesDepuisConfig | OK |
| US-10 | CA9 | xtream_test.go::TestRequeteRefuseLesSchemasAutresQueHTTP ; ::TestRedirectionVersUnAutreSchemaRefusee | OK |
| US-10 | CA10 | internal/baux/baux_test.go::TestTerminerLiberePartoutEtReveilleLAttente | OK |
| US-10 | CA11 | internal/live/relais_test.go::TestUneSeulePatienceBauxPuis407 | OK |
| US-11 | CA1 | internal/api/api_test.go::TestContratEtRoutesCorrespondent ; ::TestContratCoherentAvecLesNiveauxDAcces (erreurs {code,message} : ::TestFournisseurEnErreurDonne502SansDetail) | OK |
| US-11 | CA2 | api_test.go::TestChainesPaginationFiltreRecherche ; ::TestCategories ; ::TestFilmsSeriesSaisonsEtRecherche ; ::TestFusionDeFournisseurs ; ::TestETagEtIfNoneMatch | OK |
| US-11 | CA3 | api_test.go::TestContratEtRoutesCorrespondent | OK |
| US-11 | CA4 | api_test.go::TestRefusParDefautSurToutesLesRoutes ; ::TestCheminsEtMethodesInconnusRefuses ; ::TestRoutesAdminRefuseesAuxOrdinaires | OK |
| US-11 | CA5 | api_test.go::TestCorpsTropGros ; ::TestServeurAvecLesDelaisDeLaConfig ; ::TestPaginationEtParametresInvalides | OK |
| US-11 | CA6 | api_test.go::TestEntetesDeSecurite ; ::TestCORSFermeParDefaut ; ::TestCORSOriginesListees ; ::TestEcritureInterSitesRefusee | OK |
| US-11 | CA7 | internal/live/dette_test.go::TestEtatReserveAuxAdministrateurs ; api_test.go::TestChainesLimiteesAuFournisseurDuDirect | OK |
| US-11 | CA8 | dette_test.go::TestJetonDeSegmentRefuseSurUneAutreChaine ; ::TestSegmentVersAdressePriveeRefuse ; ::TestSegmentVersHotePublicOuAutorise ; ::TestHoteDuFournisseurToujoursAutorise ; ::TestReglagesTechniquesObligatoires ; ::TestReglagesTechniquesAppliques ; hls_test.go (jeton lie) | OK (SSRF par redirection : ECART connu) |
| US-11 | CA9 | api_test.go::TestProfilEtatIsoleParProfilActif ; ::TestProfilHistoriqueEtReglages ; ::TestHistoriqueEpisodeTransmetLaSerie ; ::TestHistoriqueEpisodeVisibleAvecLesVraisStockages ; ::TestLectureVODIdentifieLaSession ; ::TestEPG ; ::TestProfilsEtMotDePasseViaAPI | OK |
| US-13 | CA6 (routes) | api_test.go::TestProfilEtatIsoleParProfilActif ; ::TestProfilRequisQuandAucunProfilActif ; ::TestLimiteDeProfilEn422 | OK |
| US-14 | CA1 | internal/telemetrie/telemetrie_test.go::TestMetriquesAuFormatPrometheus ; TestRoutesProtegees ; internal/live/relais_test.go::TestInstrumentationDuRelais ; ::TestInstrumentationDuRelaisSecoursEtErreur ; internal/baux/baux_test.go (instrumentation) ; internal/catalogue/synchro_test.go (instrumentation) ; internal/epg/service_test.go (instrumentation) | OK |
| US-14 | CA2 | telemetrie_test.go::TestCardinaliteBornee ; ::TestEtiquettesInterdites ; ::TestNomsDeMetriquesBornes ; instrum.VerifierBornees dans chaque module | OK |
| US-14 | CA3 | telemetrie_test.go::TestEvenementsDuClientEtDuServeur ; ::TestAucuneDonneeSensible ; ::TestMasquerTable ; ::TestConservationLecturePurgeEtRotation ; ::TestTailleTotaleBornee ; ::TestRepriseApresArretBrutal ; ::TestLimiteDeTailleDeLEnvoi ; ::TestDebitParAppareil | OK |
| US-14 | CA4 | telemetrie_test.go::TestInactifCoutNul (limite Go : etiquettes variadiques allouent chez l'appelant) | OK (reserve) |
| US-14 | CA5 | domain.Instrumentation ; telemetrie_test.go::TestPersonneNImporteTelemetrie ; aucun import de telemetrie par live/baux/vod | OK |
| US-14 | CA6 | telemetrie_test.go::TestRoutesProtegees ; internal/api/api_test.go::TestRoutesAdminRefuseesAuxOrdinaires | OK |
| US-14 | CA7 (SourceEPG) | internal/xtream/xtream_test.go (URLEPG, test ajoute) | OK |
| US-12 | CA1 | cmd/mytv/main_test.go::TestVerifierConfig ; ::TestDemarrageEchoueAvecToutesLesErreurs ; internal/appli/appli_test.go::TestVerifierRassembleToutesLesErreurs | OK |
| US-12 | CA2 | internal/appli/appli_test.go::TestArretPropreSansGoroutineRestante ; ::TestArretCoupeLesRequetesLentesApresLeDelai ; cmd/mytv/main_test.go::TestArretSurSignal ; ::TestSuppressionDUnProfilEffaceSonEtat ; ::TestTelemetrieCableeEtProtegee | OK |
| US-12 | CA3 | appli_test.go::TestBoutEnBoutConnexionChainesDirectZapping ; ::TestAdministrateurLitEtat | OK |
| US-12 | CA4 | appli_test.go::TestSanteSansAuthentificationNiDetail | OK |
| US-12 | CA5 | appli_test.go::TestBoutEnBoutConnexionChainesDirectZapping (journal JSON sans secret) | OK |
| US-12 | CA6 | appli_test.go::TestEcouteUniquementSurLAdresseConfiguree ; ::TestSecretDeSignatureTropCourtRefuse | OK |
| US-12 | CA7 | docs/exploitation.md (reprise non persistante documentee) ; pas de test de redemarrage apres echec | ECART (documentee seulement) |
| US-12 | CA8 | test de bout en bout du binaire par le coordinateur (voir ci-dessous) | OK (manuel, non automatise) |

## Couverture de lignes (fin du lot 2, `go test -cover ./...`)
api 83,6 · appli 81,2 · basesqlite 79,6 · baux 94,2 · catalogue 90,9 · cmd/mytv 81,5 · cmd/mytv-comptes 81,4 · comptes 82,1 · config 94,5 · epg 88,6 · fauxiptv 93,7 · hls 95,8 · live 92,7 · profil 82,0 · remux 93,8 · stockage 85,6 · telemetrie 92,5 · vod 92,6 · xtream 89,6. Aucun paquet sous 70 %.

## Test de bout en bout du binaire (coordinateur, 2026-10-15)
Binaire `mytv` compile, lance avec une config d'exemple contre le faux fournisseur autonome (127.0.0.1, mode une seule connexion), base SQLite temporaire dans /opt/mytv-work/e2e ; aucun hote reel contacte. Constats : `--verifier-config` OK ; creation de l'admin par `mytv-comptes` ; synchro au demarrage (2 chaines) ; /sante sans detail ; /chaines sans cookie 401, mauvais mot de passe 401 `identifiants_invalides`, connexion 200 (cookie HttpOnly SameSite=Lax) ; liste des chaines ; playlist HLS reecrite (URL de segments signees) ; segment 200 (4096 o) ; meme jeton sur une autre chaine 403, sans cookie 401 ; zapping 100 -> 200 sans 407 ; /live/etat 200 pour l'admin ; /metriques expose 73 lignes live_/baux_ ; brute force : 4 echecs puis 429 ; deconnexion 204 puis 401 ; aucun secret (mot de passe, signature, identifiants du fournisseur) dans le journal ; arret SIGTERM propre ("arret demande", "arrete"). Note : le faux ne publie pas d'EPG (404 journalise, ancien EPG conserve).

## Verification de l'inventaire TuliProx (fonctionnalites conservees)
| Fonctionnalite | Couverture | Etat |
|---|---|---|
| API catalogue : categories, chaines, films, series | US-11 CA2 (TestChainesPaginationFiltreRecherche, TestCategories, TestFilmsSeriesSaisonsEtRecherche) ; US-01, US-03 | OK |
| Recherche | US-01 CA4 (FTS accents/casse) ; /recherche US-11 | OK |
| EPG (programmes, fuseau) | US-09 CA1-CA7 ; route TestEPG ; fuseau epg.fuseau (TestAnalyseFuseauParDefaut) | OK, MAIS fournisseur reel : URLEPG (xmltv.php) jamais verifie contre le vrai fournisseur |
| Live HLS avec relais | US-06, US-07, TestBoutEnBoutConnexionChainesDirectZapping | OK (mono-fournisseur) |
| VOD avec Range | US-10 CA1 (TestDirectRange...) ; remux : pas de 206 | OK (reserve remux) |
| Comptes | US-08 | OK ; pas d'import des comptes existants |
| Favoris / historique | US-13 + routes US-11 | OK ; pas d'import de l'ancien etat |
| Telemetrie | US-14 | OK |
| Rafraichissement parametrable | US-02 CA2-CA3, US-09 CA6, config | OK |
Manque : migration/import des donnees de l'ancien service (comptes, favoris, historique) ; interface (SolidJS /opt/mytv-ui) non adaptee a l'API v1 ; HEVC non transcode.

## Ecarts
Aucun CA du lot 1 fusionne sans test. Reserves :
- US-04 CA3 : les pannes 458 et 405 ne sont verifiees que via les tests du client Xtream (pas de test propre au faux).
- US-10 : le remux n'offre pas de 206 (seek par ?t=) ; pas de transcodage video (HEVC => 415). Les URL amont (avec identifiants Xtream) passent en argument de ffmpeg, donc visibles dans `ps` sur la machine hote : risque accepte, a documenter.
- Direct mono-fournisseur (voir US-11).
- US-11 CA8 : le filtre d'hotes (SSRF) ne controle que l'URL initiale d'un segment ; une redirection du fournisseur vers une adresse privee est suivie (docs/contexte.md). DNS rebinding : liste blanche de noms recommandee.
- US-12 CA7 : pas de test de redemarrage apres echec de synchro (reprise non persistante, documentee).
- US-12 CA8 : le test de bout en bout du binaire est manuel (script ad hoc dans /opt/mytv-work/e2e, non versionne) ; la version automatisee est internal/appli::TestBoutEnBoutConnexionChainesDirectZapping.
- Journal : `reprise_dans` est ecrit en nanosecondes (cosmetique).
- US-03 CA4 : delai_connexion est desormais teste (TestDelaiConnexionBorneLaPoigneeDeMain).


## Lot 3 (US-15 a US-22)
| US | CA | Test | Statut |
|----|----|------|--------|
| US-15 | CA1-CA4 | internal/dev/dev_test.go::TestCA1_DemarrageComplet ; ::TestCA1_CA2_VolumeParDefautEtDemarrageRapide ; ::TestCA2_ArretPropre ; ::TestCA2_AucuneConnexionHorsBoucleLocale ; ::TestCA2_RefuseLesAdressesNonLocales ; ::TestCA3_ServiInterfaceMemeOrigine ; ::TestCA4_PanneProgrammee ; ::TestCA4_Latence ; cmd/mytv-dev/main_test.go | OK |
| US-16 | CA1 | internal/api/export_test.go::TestCA1_ExportNDJSONConformeAuContrat ; ::TestCA1_ExportETagLieALaSynchro ; ::TestCA1_ExportVolumeEnMoinsDeTroisSecondes (25 000 + 40 000, ~0,4 s) ; ::TestCA1_ExportEnFluxSansMaterialisation ; internal/stockage/export_test.go::TestCA1_ExportIsoleDUneResynchronisation | OK |
| US-16 | CA2 | internal/api/grille_test.go::TestCA2_GrilleChainesInconnuesOuSansGuide ; ::TestCA2_GrilleDeuxCentChainesSixHeures | OK |
| US-16 | CA3 | export_test.go::TestCA3_AjouteChainesEtSeries ; stockage::TestMigrationAjouteSurBaseExistante | OK |
| US-16 | CA4-CA6 | export_test.go::TestLimitesDansCompte ; ::TestFinDeLecture ; baux::LibererSi ; internal/vod/vod_test.go (HEAD sans ffmpeg, X-Mytv-Mode/Duree) ; appli_test (ids series s<numero>, Session.nom) | OK |
| US-17 | CA1 | internal/remux/exec_test.go + entree_test.go + fluxsonde_test.go (aucun hote/identifiant dans argv) | OK, limite : mp4 avec moov en fin non remuxable par pipe:0, jamais essaye avec un vrai ffmpeg |
| US-17 | CA2 | internal/hotes/hotes_test.go (redirections, resolveur factice, rebinding) ; internal/xtream/hotes_test.go | OK |
| US-17 | CA3-CA4 | internal/vod/vod_test.go (pause_max, 416 + Content-Range) | OK |
| US-18 | CA1 | voir statut du lot | voir bas de section |
| US-19 | CA1-CA5 | /opt/mytv-ui tests/e2e/us19.mjs (11 scenarios) ; tests/unit/*.test.mjs (32) ; tests/dist-sans-secrets.mjs (STRICT : 0 secret, 0 reliquat) ; tests/e2e/us19-harnais.mjs (vrai mytv-dev) | OK |
| US-20 | CA1-CA5 | tests/e2e/us20-lecture.mjs (45 verifications) ; tests/unit/sans-reference.test.ts (CA5, greppe src/) | OK ; PiP et Cast reel non testables en headless |
| US-21 | CA1-CA2 | tests/e2e/us21-guide.mjs (34 verifications, fuseau +5:45 inclus) | OK |
| US-22 | CA1-CA4 | tests/e2e/us22-recette.mjs : 13 scenarios contre cmd/mytv-dev (connexion, import, recherche, fiche+favori, guide, direct, zapping x10, deconnexion, demarrage a chaud, panne 407 30 s, 502, session expiree, charge 68 000 entrees) ; toute requete hors 127.0.0.1 echoue | OK |

Mesures de la recette (US-22, machine de dev, faux fournisseur local) : demarrage mytv-dev 2,3 s ; connexion + affichage 0,3 s ; import du catalogue standard (7 500 entrees) ~0,9 s ; import de 68 000 entrees (25 000 chaines, 40 000 films, 3 000 series) 4,5 s ; demarrage a chaud 0,26 s ; zapping x10 (regle une-connexion a 2 s) 6,7 s, 8 refus 407 absorbes par le serveur, 0 vu par l'interface ; panne 407 de 30 s : lecture reprise apres 30,3 s ; guide : 2 appels de grille, 0 appel par chaine.

Ecarts du lot 3 :
- Chromium headless ne decode pas le H.264 : lecture reelle, PiP et Cast reels non verifies automatiquement (recette manuelle).
- Le zapping avec la vraie regle du fournisseur (~100 s) ne peut pas etre rapide : le serveur patiente jusqu'a `bail.patience_max` puis 503 + Retry-After ; l'interface reessaie (6 essais max). A observer en recette reelle.
- Hero « A la une » sans synopsis/genre/duree (absents du contrat v1).
- Anciens scripts Playwright (117) retires : ils ciblaient l'ancienne API et le vrai fournisseur.
- Correctif d'ui-2 dans synchro.ts (ui-1) : fusion de l'historique concurrente avec l'ouverture du lecteur.

## Lot 4 (US-23 a US-29) — corrections de la revue comparative TuliProx

Les CA sont aussi etiquetes « CAn » dans les commentaires des tests. Colonne Test = `fichier::fonction`.

| US | CA | Test | Etat |
|----|----|------|------|
| US-23 | CA1 | internal/api/image_test.go::TestCA1_LogosEtImagesReecritsPartout ; ::TestCA1_ExportNDJSONReecrit ; ::TestCA1_ETagCatalogueLieAuxJetons ; internal/images/images_test.go::TestReecritureDesURL | OK (decision : toutes les URL http(s) reecrites, https compris, car la CSP devient `img-src 'self' data:`) |
| US-23 | CA2 | internal/images/images_test.go::TestJetonFalsifieOuEtrangerRefuse ; ::TestJetonDUnAutreUsageRefuse ; ::TestJetonDUnFournisseurRetireRefuse | OK |
| US-23 | CA3 | internal/images/images_test.go::TestHoteNonAutoriseJamaisContacte ; ::TestRedirectionVersHoteInterditRefusee ; ::TestReponseNonImageRefusee ; ::TestImageTropGrosseRefusee ; ::TestRequeteAmontAnonymeAvecLAgentDuFournisseur ; ::TestErreursNeCitentNiURLNiSecret ; internal/api/image_test.go::TestCA3_HoteNonAutoriseRefuse | OK |
| US-23 | CA4 | images_test.go::TestCacheDisqueBorneEnOctetsAvecEvictionLRU ; ::TestCacheNegatifEvitedeMartelerLeFournisseur ; ::TestDemandesSimultanéesFusionnees ; ::TestCacheSurvitAuRedemarrage ; api/image_test.go::TestCA4_ImageAbsente404EtCacheNegatif ; internal/config/config_test.go::TestImagesDefautsEtValidation | OK |
| US-23 | CA5 | images_test.go::TestConcurrenceAmontBornee ; ::TestSaturationRendErrSatureSansCacheNegatif | OK (aucun bail pris : le paquet n'importe pas baux) |
| US-23 | CA6 | internal/appli/appli_test.go (CSP) ; api/image_test.go::TestCA6_ExportReecritMoinsDeTroisSecondes (25 000 chaines + 40 000 films : ~1,06 s ; +70 octets gzip par ligne) | OK |
| US-23 | CA7 | api/image_test.go::TestCA7_ImageDeBoutEnBout | OK |
| US-24 | CA1 | internal/vod/patience_test.go::TestCA1_PremierAccesPatienteSurLes407PuisReussit ; ::TestCA1_PatienceEpuiseeDonne503 ; ::TestCA3_CoupurePuis407ALaRepriseNeConsommePasLEssai ; internal/baux/patience_test.go | OK (mutation verifiee : sans patience, 4 tests echouent) |
| US-24 | CA2 | patience_test.go::TestCA2_SeekCoupeLaLectureDirecteBloqueeAvantDOuvrirLAmont ; ::TestCA2_DixSeeksDeSuiteSansAucun503 ; ::TestCA2_UnHEADNeCoupePasLaLectureEnCours | OK (mutation verifiee : sans coupure du seek, 4 tests echouent) |
| US-24 | CA3 | patience_test.go::TestCA3_ClientPartiPendantLaPatienceArreteLAttente ; ::TestCA3_FinDeLectureRendLaPlace (-race) | OK |
| US-24 | CA4 | patience_test.go::TestCA4_HEVCRefuseParDefautMemePourCast ; ::TestCA4_HEVCAvecOptionEtClientCastEstServiTelQuel ; ::TestCA4_HEVCAvecOptionMaisSansEnteteCastRestRefuse ; internal/config/config_test.go::TestVodRelaisBrutHEVC | OK (option `vod.relais_brut_hevc`, defaut faux) |
| US-25 | CA1 | internal/api/supervision_test.go::TestFournisseurCompteAdresseEtPanneSontDistingues ; internal/live/relais_test.go::TestPlaylistRefusDuCompteOuDeLAdresseEstDistingue ; ::TestSegmentRefusDuCompteEstDistingue ; supervision_test.go::TestClasseDistingueCompteAdresseEtPanne ; api/supervision_test.go::TestReponsesSupervisionConformesAuContrat | OK (mutation verifiee) |
| US-25 | CA2 | internal/supervision/supervision_test.go::TestFournisseursJSON ; ::TestStatutExpireDeUserInfoEstUnCompteInvalide ; internal/xtream/xtream_test.go (user_info) ; internal/telemetrie/telemetrie_test.go (jauge) | OK |
| US-25 | CA3 | supervision_test.go::TestSanteApprofondie ; api/supervision_test.go::TestRoutesAdminSupervisionProtegees | OK |
| US-25 | CA4 | supervision_test.go::TestWebhookExpirationProcheSigneEtAntiRafale ; ::TestWebhookEchecsRepetesApresNEssais ; ::TestWebhookCompteRefuseEtRetourALaNormale ; ::TestWebhookLentOuEnEchecNeBloquePasEtNeFuitRien ; ::TestWebhookNeSuitPasLesRedirections ; config_test.go::TestSupervisionDefautsEtValidation | OK (mutation anti-rafale verifiee) |
| US-25 | CA5 | /opt/mytv-ui tests/e2e/us25-fournisseur.mjs (37 verifications) ; tests/unit/abonnement.test.ts, errors.test.ts, flux.test.ts | OK (rejoue par le coordinateur) |
| US-26 | CA1 | internal/sauvegarde/sauvegarde_test.go::TestCopieCoherenteSansArreterLesEcritures ; ::TestCopieCorrompueNEstPasPubliee ; ::TestConservationPurgeLesVieillesSauvegardesSeulement ; ::TestPlanifieeChaqueJourALHeure ; ::TestEchecJournaliseEtSignale | OK |
| US-26 | CA2 | cmd/mytv/main_test.go::TestSauvegarderPuisRestaurerDansUneBaseNeuve | OK |
| US-26 | CA3 | cmd/mytv/main_test.go::TestVerifierConfigAvertitPourUneEcouteSurToutesLesInterfaces ; internal/config (validation serveur.ecoute) | OK |
| US-27 | CA1-CA2 | internal/xtream/connexions_test.go::TestCA1_LesRequetesDeFluxAnnoncentConnectionClose ; ::TestCA2_AucuneConnexionOuverteApresUneLectureDeFlux ; ::TestCA2_UneRedirectionNeLaissePasDeConnexion ; config_test.go::TestReseauFermerConnexionsFlux | OK (mutation verifiee) |
| US-28 | CA1-CA4, CA6 | voir la section « US-28 » ci-dessous | OK |
| US-28 | CA5 | interface : voir la section US-28 UI | voir bas de section |
| US-29 | CA1 | internal/epg (TestCA1_AnalyseLitLesIconesDesCanaux ; TestCA1_LogoDuGuideEnRepliSansEcraserLeFournisseur) ; internal/api (logo de repli reecrit par le proxy) | OK |
| US-29 | CA2 | internal/epg::TestCA2_SousTitreEpisodeEtAnneeConserves ; ::TestEpisodeDepuisXMLTVNS | OK |

Recette de bout en bout du lot 4 (coordinateur, contre cmd/mytv-dev construit depuis main, faux fournisseur, 127.0.0.1, ports ephemeres) :
- `npm run test:recette` (US-22) : 12 scenarios OK ; import 7 500 entrees 955 ms ; import de 68 000 entrees 4,8 s ; demarrage a chaud 218 ms ; zapping x10 6,7 s (8 refus 407 absorbes) ; panne 407 de 30 s : reprise apres 30,4 s ; guide : 2 appels de grille ; details : 1 appel get_vod_info + 1 get_series_info pour toute la session (0 par carte de liste).
- us19 : 11/11 (les chemins `/app/` du test etaient perimes depuis la base `/` : corriges) ; us20-lecture 45/45 ; us21-guide 34/34 ; us28 8/8 ; us25-fournisseur 37/37 ; us19-harnais et us28-harnais OK ; tests unitaires UI 67/67 ; `npm run build` vert ; dist : 29 fichiers, 0 secret, 0 reliquat.
- Non verifie automatiquement : l'affichage des logos servis par le proxy dans le navigateur (le faux fournisseur n'expose pas de picons) : a regarder apres deploiement.

Couverture de lignes en fin de lot 4 (`go test -cover ./...`) : api 86,8 % ; appli 82,5 % ; baux 94,5 % ; catalogue 89,5 % ; cmd/mytv 89,1 % ; comptes 82,1 % ; config 95,5 % ; epg 89,7 % ; hls 94,4 % ; hotes 91,7 % ; images 85,4 % ; live 93,9 % ; metadonnees 82,5 % ; profil 82,0 % ; remux 90,8 % ; sauvegarde 80,3 % ; stockage 84,4 % ; supervision 88,0 % ; telemetrie 92,4 % ; vod 92,8 % ; xtream 90,3 %.

Ecarts du lot 4 :
- Aucun test contre le vrai fournisseur (interdit) : le comportement reel du 407 apres seek, de `Connection: close` (le fournisseur lie-t-il vraiment sa place a la connexion TCP ?), de user_info (`exp_date`, `status` reels) et des picons reste a observer apres deploiement.
- US-23 : la connexion d'un hote d'image est controlee par le meme filtre que les segments ; si un hote de picons est en reseau prive il faut l'ajouter a `serveur.hotes_segments_autorises`.
- US-24 CA2 : un seek coupe aussi une lecture directe concurrente de la meme ressource (deux requetes Range paralleles d'un meme lecteur se remplacent) ; voulu (une seule connexion amont). Observe en lecture reelle : rafales de 499 sur les plages concurrentes legitimes du navigateur ; corrige par la grace `vod.grace_annulation` (lot 7).
- US-25 : `GET /admin/sante` interroge le fournisseur (user_info, sans flux) a chaque appel : a ne pas sonder a haute frequence.
- US-26 : sauvegarde activee par defaut a 03:30 (14 jours) ; la restauration est manuelle (procedure dans docs/exploitation.md).
- Chromium headless ne decode toujours pas le H.264 : lecture reelle, PiP et Cast non verifies automatiquement.

## US-28 — Metadonnees des films et des series (lot 4, partie Go)

| US | CA | Test | Etat |
|----|----|------|------|
| US-28 | CA1 | internal/xtream/details_test.go (complet, tolerant, sans fiche, valeurs dangereuses, erreurs typees, lent, saisons + episodes, `info: []`) | OK |
| US-28 | CA2 | internal/stockage/details_test.go (migration depuis la v3, jamais d'appauvrissement, survit a la resynchro, concurrence, purge) ; internal/metadonnees (perime redemande, echec ne vide pas) ; internal/config (duree_vie, validation) | OK |
| US-28 | CA3 | internal/api/details_test.go (details, cache, `?details=non`, ETag, listes sans appel, 8 ouvertures = 1 appel, identifiant inconnu jamais transmis, saisons + details = 1 appel, conformite des champs au contrat) ; internal/appli (bout en bout) | OK |
| US-28 | CA4 | internal/metadonnees (coupe-circuit 401/403/458 et sonde unique, concurrence bornee, debit borne, pause pendant l'import, delai court, degradation) ; internal/api (fiche repondue malgre 502/404/458/401/lent) | OK |
| US-28 | CA6 | internal/testutil/fauxiptv (get_vod_info/get_series_info, champs manquants, 404, lent, 458, pic simultane) | OK |
| US-28 | CA5 | interface : voir la remise `c4-d.US-28-ui` | a venir |


## Lot 5 (US-30 a US-33) — conversion du direct, controle de la regression vs mytv-flux

Reference : /opt/mytv-flux/app (strategie.py `decider`, main.py `route_live`/`live_transcode`/`forcage`, sonde.py, config.py). Aucune fonction du DIRECT n'est perdue ; ecarts releves (et decides) :

| Ancien comportement | mytv apres le lot 5 | Ecart |
|---|---|---|
| Sonde ffprobe de la playlist (URL fournisseur dans argv de ffprobe) | ffprobe lit UN segment (<= 8 Mio) par une source locale ; aucun identifiant en argv | AMELIORATION (securite) |
| Cache de sonde 600 s | `live.duree_sonde` 10 min, cache par chaine, verdict perime servi puis resonde derriere | identique |
| Echec de sonde -> 502 | repli sur le relais brut, pas de nouvelle tentative avant `live.echec_sonde` | VOULU (US-30 CA1) |
| h264+aac -> playlist reecrite | `direct` (relais d'origine) | identique |
| audio ac3/eac3/dts/truehd/mp2/inconnu -> copie video + AAC 192k stereo | `audio`, memes arguments ffmpeg | identique |
| audio acceptes : aac, mp3, opus, vorbis, flac | aac, mp3 seulement ; opus/vorbis/flac en TS sont donc convertis en AAC | ECART MINEUR (surcout negligeable, ~2,5 % d'un coeur) |
| HEVC/non h264 <= 1080p (ou hauteur inconnue) -> x264 veryfast crf 23, g 100 | `video`, memes arguments | identique |
| HEVC > 1080p (4K) -> refus 415 pour tous | refus 415 `format_hevc_non_lisible` SEULEMENT pour un navigateur sans capacite `hevc` ; avec `hevc` : direct, aucun ffmpeg | EVOLUTION VOULUE (negociation des capacites) |
| Pas de notion de capacites : EAC3 toujours converti | `eac3`/`ac3` annonces -> lus tels quels | EVOLUTION VOULUE |
| Reencodage desactive -> refus avec message | idem (`live.reencodage_video`), message nommant codec et hauteur | identique |
| `?forcer=remux|audio|video` | `?forcer=audio|video` ; `remux` -> 400 `forcer_invalide` (le direct n'a pas de remux) ; `video` refuse si reencodage interdit ou 4K sondee | ECART MINEUR (remux n'avait pas de sens pour un HLS) |
| Dossier `hls/<chaine>` reutilise, nettoye au redemarrage de la session | dossier aleatoire par transcodage `<chaine>-<12 hexa>` + prefixe de segment ; orphelins supprimes au demarrage ; groupe de processus tue | AMELIORATION |
| Segments servis par nom (`/flux/hls/<id>/<nom>`) | jetons signes lies a la chaine, URL stables | AMELIORATION |
| Attente de 2 segments, 30 s max, 502 si ffmpeg meurt | idem (`live.delai_demarrage_transcodage` 30 s, 502 `transcodage_impossible`) | identique |
| Inactivite 45 s | `live.inactivite_transcodage` 60 s | ECART MINEUR (reglable) |
| Un transcodage a la fois (CRENEAU), zapping tue l'ancien | idem + bail renouvele tant que ffmpeg tourne ; `remux.processus_max` partage avec les films | identique |
| Changement de mode sur la meme chaine relance ffmpeg | idem | identique |
| Playlist maitresse : ffmpeg choisit la variante | premiere variante (sonde et amorce) | ECART MINEUR (documente) |

CA2 (zapping x10) : `internal/live/zapping_test.go` — dix zappings 201 (video) / 202 (audio) / 203 (direct) sur le faux fournisseur (127.0.0.1) : jamais plus d'un ffmpeg/ffprobe vivant, un seul dossier, bon mode ; apres la fin de lecture, aucun processus ni dossier. Patience 407 : tests existants du direct inchanges (verts) et `TestTranscodagePatienteSur407`.

Risques releves (non bloquants) : (1) la sonde en arriere-plan telecharge un segment pendant que le lecteur en demande d'autres : sur le vrai fournisseur (une seule connexion) cela peut provoquer un 407 transitoire — le client filtre applique la patience ; verifier apres deploiement et, au besoin, allonger `live.duree_sonde`. (2) Premiere lecture d'une chaine inconnue : jusqu'a `live.delai_sonde` (8 s) de plus. (3) Le mode `video` occupe ~1 coeur tant qu'il est regarde.

## Lot 6 — Compte utilise ailleurs (US-34, US-35)

| US | CA | Couvert par | Etat |
|---|---|---|---|
| US-34 | CA1 retrait progressif (3 s, x2, plafond 30 s, gigue, <= 5 requetes en 40 s) | `baux/patience_test.go` (TestCA1_*), `live` TestCA1_SansRetraitProgressif..., `vod` TestCA1_Film407... | OK, mutation verifiee |
| US-34 | CA2 503 + Retry-After 15/30/60/60, RAZ apres succes, `X-Mytv-Erreur` | `occupation_test` TestCA2_*, `api_test` TestCA2_Vod503..., `live` TestCA2_LeRefusNeDeclencheAucunAppelEnPlus | OK, mutation verifiee (constant, sans RAZ) |
| US-34 | CA3 diagnostic user_info (cache, singleflight, classification) + contrat | `occupation_test` TestCA3_*, `live` TestCA3_*, conformite openapi | OK, mutations cache/singleflight/ailleurs/par_mytv |
| US-34 | CA4 prise de place (WARN, `live_prises_de_place_total`, `/admin/fournisseurs`) | `occupation_test` TestCA4_*, `live` TestCA4_* | OK, mutation verifiee |
| US-34 | CA5 faux fournisseur (occupant externe, active_cons) et 4 scenarios | `fauxiptv_test`, `live/occupation_test` TestCA5_* | OK |
| US-34 | CA6 pas de regression / cout nul sans 407 | `live` TestCA6_SansRefusAucuneLectureDeUserInfo, zapping x10 existant | OK |
| US-35 | CA1 a CA4 (messages, decompte, reprise, plafond, coupure, telemetrie) | `tests/unit/machine-occupation.test.ts`, `tests/e2e/us35.mjs` (29/29) | OK, mutations Retry-After et requete en vol |

Ecarts et risques du lot 6 :
- Le vrai fournisseur renvoie toujours `active_cons: 0` : `compte_utilise_ailleurs` ne sort donc pas de user_info en production ; la classe observee sera le plus souvent `compte_occupe` (reessai automatique). La prise de place en pleine lecture reste visible dans `GET /admin/fournisseurs` (`occupation: ailleurs`) et le journal. Amelioration possible : classer `ailleurs` quand `Etat()` l'est deja.
- Cote UI, un 403/407 de segment sans code serveur apres des images est traite comme « compte utilise ailleurs » (coupure) ; hls.js ne reessaie plus en silence un statut 403/407/429/503 (reprise par la machine d'attente, plafond 10 min fixe dans le code).
- `GET /admin/fournisseurs` gagne trois champs requis (`occupation`, `depuis`, `derniere_prise`).

Recette Playwright rejouee contre cmd/mytv-dev apres fusion : `test:recette` 12/12 OK (import complet 879 ms, zapping x10 6,8 s sans 407 residuel, panne 407 de 30 s absorbee en 34,6 s, grosse charge 68 000 entrees en 6,6 s) ; us20 45/45, us25 37/37, us32 30/30, us35 29/29, unit UI 97/97 ; aucune requete hors 127.0.0.1.

## Lot 7 — Coupures sans reprise des films remuxes (relecture independante)

| Constat | Correction | Couvert par | Etat |
|---|---|---|---|
| 1. Troncature invisible du remux (chunked : fin de film au lieu d'erreur) | `vod.Servir` abandonne une reponse entamee qui echoue (`panic(http.ErrAbortHandler)`), journal « coupee (pause) » / « en echec » | `vod_test.go::TestRemuxMortEnCoursDeRoute`, `::TestRemuxAmontCoupeApresLesReprisesTronqueLaReponse` (vrai lanceur, client net/http reel), `::TestSeekRemplaceLeRemuxEnCours`, `::TestCA3_RemuxEnPauseArreteFfmpegEtRendLaPlace` | OK, mutation verifiee (sans la panique : les deux tests de coupure echouent ; pause et seek etaient deja tronques par le delai d'ecriture echu) |
| 2. Annulation immediate des plages concurrentes de la meme ressource (499 en rafale) | grace `vod.grace_annulation` (250 ms) pour une lecture directe de la meme ressource ; autre ressource et remux annules tout de suite | `grace_test.go::TestGrace_*`, `config_test.go::TestVodGraceAnnulation` | OK, mutation verifiee (annulation immediate : 2 tests sur 3 echouent) |
| 3. HEAD direct ouvrait un GET amont complet puis le fermait (une connexion fournisseur jetee avant celle du `<video>`) | `direct.go::directTete` : un seul octet demande (`Range: bytes=0-0`), plage du client calculee localement, 416 hors fichier | `vod_test.go::TestDirectHEADNeDemandeQuUnOctetAuFournisseur` (traces du faux : Range amont), `::TestDirectHEAD`, `::TestCA6_*` | OK, mutation verifiee (GET complet : le test echoue sur les 4 HEAD) |

