Oh yes, this project is very AI-coded, with all the pros and cons that come
with it. The bad news is the quality is currently variable; the good news is
that this project would not have been possible at all without it, because
it's too many moving parts at once to be justifiable as a human-driven
experiment. Over time we need to improve the quality of all the components.

# Claude Code Project Notes

- Always run "make test" after making changes. "make test" also enforces gofmt;
  run "gofmt -w ." to fix formatting before committing.
- To build: "make binaries" (puts them in bin/) or "make ts" (just the ts binary)
- e2e tests MUST NEVER SKIP. "make e2e" must either fully pass every validation
  or fail. Never add t.Skip/t.Skipf/t.SkipNow to e2e tests; if a precondition
  (root, btrfs, VM deps) is missing, that is a misconfigured environment and the
  test must fail (t.Fatal), not skip. The e2e package is built with the "e2e"
  build tag so it is excluded from "make test" and only run via "make e2e".
- e2e tests run simplest-to-hardest in tiers (see e2e/main_test.go TestMain); if
  an early tier fails, later tiers are skipped so you debug the root cause first.
- To run a single e2e test, use E2E_ARGS with -test.run and log to a file:
  `make e2e E2E_ARGS="-test.run=TestSSHContainerBasic" 2>&1 | tee e2e.log`
  Always log output to a file (e2e tests are verbose). Tests typically complete
  in ~30s; use a 1-2 minute timeout when waiting.
- e2e test prerequisites (install if missing — tests will t.Fatal, not skip):
  - `busybox-static` (NOT `busybox` — the dynamic one can't exec inside
    nil:nil:nil frames which have no /lib64; `apt-get install busybox-static`)
  - `virtiofsd` and `passt` for VM tests (`apt-get install virtiofsd passt`)
  - `/dev/kvm` for VM tests (not available inside containers without KVM
    passthrough; the outer host must propagate it)
  - VM binaries (`cloud-hypervisor`, `vmlinux`) in `vm/` or set
    `THUNDERSNAP_VM_DIR`
- Running e2e tests inside a thundersnap container (nested development):
  - Works, but requires the btrfs+setns fixes in `cmd/ts/` (see
    `docs/nested-btrfs-setns.md` for the full explanation)
  - The cgroup warnings (`failed to create parent cgroup`) are expected and
    harmless — cgroup setup is best-effort and `/sys/fs/cgroup` is read-only
    inside containers
  - VM tests need `/dev/kvm` propagated into the container
- This project workspace may be using the 'jj' tool instead of git. Always
  check for jj first. If using jj, when making a fix, always `jj describe` it
  when done and then `jj new` so you don't accidentally mix changes
  together. Be careful that `jj log` shows not just this branch, but other
  branches mixed into the hierarchy!
