# Containers Anywhere 0.3.2 (beta)

Beta, not production-ready. The app and the appliance both change, so update both. K3s is
unchanged (1.35.8), so the appliance can be updated with a root swap.

## What's new

- **A namespace can publish its own bare name.** A `WorkloadPublication` in namespace `care`
  may now use `care.local` / `care.localhost` as well as `<name>.care.local`. Another namespace
  still cannot claim it, and the namespace `demo` is excluded because `demo.local` is the
  built-in demo. Admission is enforced in the appliance, and the app validates the same rule, so
  an app older than 0.3.2 rejects a snapshot that contains a bare-name publication. Update the app first.

## Verification

Unit tests, and a dry-run admission check added to the publication acceptance suite
(bare name admitted, another namespace's name refused). See the CI run for whether the
end-to-end and extended suites ran.
