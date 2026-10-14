//go:build !race

package stockage

// Le detecteur de courses ralentit le code de 5 a 20 fois : les seuils de duree
// du CA7 ne sont verifies que hors -race (le benchmark, lui, mesure toujours).
const verifierSeuils = true
