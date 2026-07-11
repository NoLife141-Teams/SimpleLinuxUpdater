package main

import healthpkg "debian-updater/internal/health"

func testHostHealthObservation() healthpkg.SQLiteObservation {
	return healthpkg.SQLiteObservation{DB: getDB}
}

func saveServerFacts(record serverFactsRecord) error {
	return testHostHealthObservation().AcceptCollectedFacts(record)
}

func loadServerFacts() (map[string]serverFactsRecord, error) {
	return testHostHealthObservation().Latest()
}
