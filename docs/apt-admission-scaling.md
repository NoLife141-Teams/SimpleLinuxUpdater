# APT notification admission scaling

An APT admission records that a given incident and destination already entered the notification outbox. It survives the 30-day outbox history retention so restoring an older installation backup cannot deliver the same incident again. The condition worker ordinarily polls once per second and revalidates conditions before delivery.

## Reproduce the measurements

```sh
go test ./internal/notifications -run '^$' -bench '^BenchmarkAPTAdmissionLedger$' \
  -benchmem -benchtime=1s -count=3 -cpu=2
```

The fixture loads 100, 1,000, 10,000 or 50,000 synthetic historical admissions with no retained outbox rows for them. Every run has one active incident and a configured webhook. The reconciliation benchmark measures a steady-state poll after admitting that incident. The restore benchmark measures capturing the live notification state and staging its evidence into a fresh disposable SQLite copy. The copy contains one historical pending row that must stay suppressed. Setup, benchmark fixture migration, archive encryption and decryption, and runtime reload are outside the measured operations.

The JSON measurements used the runtime at `48bcfa844dca188554d5180e3615bfbc644c3945`. To rerun them, copy `internal/notifications/apt_admissions_bench_test.go` into a checkout of that revision and change the first argument of `newConditionHarness`, `openOutboxTestDB`, and `prepareOutboxSettings` from `*testing.T` to `testing.TB` in their test declarations. Run the same command there.

## Results

Medians of three runs on Linux amd64, Go 1.26.11, modernc.org/sqlite 1.62.0, and a KVM runner on an AMD Ryzen 9 9950X. Memory columns show bytes allocated per operation. Live SQLite used one connection, WAL, and normal synchronous mode. The synthetic condition source keeps the active set fixed and isolates notification persistence costs from the rest of the application.

### Steady-state reconciliation

| Historical admissions | JSON ms/op | SQLite ms/op | JSON B/op | SQLite B/op | JSON allocs/op | SQLite allocs/op |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 100 | 0.237 | 0.041 | 126,223 | 11,919 | 761 | 156 |
| 1,000 | 1.689 | 0.043 | 1,292,871 | 11,917 | 6,190 | 156 |
| 10,000 | 21.048 | 0.045 | 15,509,040 | 11,917 | 60,344 | 156 |
| 50,000 | 121.998 | 0.050 | 69,656,417 | 11,918 | 300,732 | 156 |

### Prepare and stage restore evidence

| Historical admissions | JSON ms/op | SQLite ms/op | JSON B/op | SQLite B/op | JSON allocs/op | SQLite allocs/op |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 100 | 24.370 | 10.706 | 1,039,635 | 148,551 | 8,579 | 2,651 |
| 1,000 | 59.713 | 14.617 | 10,201,263 | 1,277,252 | 82,469 | 20,820 |
| 10,000 | 433.301 | 50.686 | 111,192,106 | 12,714,794 | 821,123 | 200,905 |
| 50,000 | 2,029.325 | 186.324 | 522,033,128 | 64,575,728 | 4,103,285 | 1,002,715 |

## Storage and compatibility

At 10,000 admissions the JSON format allocated about 15.5 MB on every poll, with further reconciliation before deliveries. The primary key in `notification_apt_admissions` makes steady-state lookups depend on the active incidents and their eligible destinations. A restore still copies historical identities whenever it carries live evidence forward.

Schema setup moves the legacy JSON keys to the indexed table and removes the migrated entries in one transaction. New admissions commit with their outbox rows. A restore unions the live and archived keys in the staged database before replacement, carries retained outcomes forward, and suppresses pending archived deliveries already admitted on the live installation. Restoring legacy JSON archives runs the same migration. Keys contain the hashed incident identity and destination fingerprint. Destination credentials remain in the existing encrypted settings.
