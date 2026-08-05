# Thundersnap MCP friction report

Date: 2026-08-03

## Context

I used the Thundersnap tools to clone `github.com/apenwarr/isochronous` into the `deb` frame, install build dependencies, compile `isoping`, run it against `apenwarr.ca`, observe its output incrementally, and stop it after collecting a sample.

## Friction encountered

### [DONE] I issued dependent jobs too early

I initially submitted repository inspection, compilation, and dependency installation together through `multi_tool_use.parallel`. Those operations were not independent: compilation required both the clone and the packages to finish first. As a result, the first build job ran before `make` and `file` were installed and failed immediately.

The API documentation correctly says that jobs in a launch array run concurrently, but it was still easy to overuse parallelism when trying to reduce latency. A stronger warning or example stating “do not parallelize clone → install → build chains” might help.

### No-argument `isoping` started a server instead of showing help

After the successful build, I ran `./isoping 2>&1 | head -100`, expecting usage output. In this program, no arguments means server mode, so it ran forever. This was application-level confusion rather than a Thundersnap defect, but it interacted with the jobs API: the foreground-style call timed out while leaving the job running, and I then had to kill it explicitly.

I learned that a `wait` timeout is only an observation timeout and never terminates the process. That behavior is documented, but it is important enough that it may deserve extra prominence in returned timeout results, perhaps with text such as “job remains running; call jobs_kill to stop it.”

### [DONE] I malformed several `view` calls

I tried to view ordinary files while passing `stream`, which is valid only for job logs. I also populated mutually exclusive or irrelevant fields with empty values (`job_id: ""`, `tail_lines: 0`) rather than omitting them. The calls failed with `stream is only valid with job_id`.

The distinction is:

- To view a file, pass `frame`, `path`, and either `view_range` or `tail_lines`; omit `job_id` and `stream` entirely.
- To view a job log, pass `job_id` and optionally `stream` or `tail_lines`; omit `frame`/`path` unless the schema explicitly permits an ignored frame.

This was the clearest API usability problem. JSON-schema clients often encourage filling every property, and empty strings are not treated as absent. Separate tools such as `thundersnap_view_file` and `thundersnap_view_job`, or a discriminated union schema, would make misuse harder. More specific validation—“for file viewing, omit stream and job_id; empty values still count as supplied”—would also help.

### [DONE] The large parallel tool call amplified mistakes

I bundled too many calls into one `multi_tool_use.parallel` invocation, including duplicate malformed `view` attempts. Since all calls were dispatched together, I could not use the first error to correct the later calls. This created noisy failures and unnecessary work.

I learned to use parallel calls only for genuinely independent, already-understood operations. For a new API or a dependency chain, smaller sequential calls are safer and usually faster overall because errors do not multiply.

### [DONE] Incremental job revisions required careful tracking

For the long-running `isoping` client, `jobs` returned a conversation-level `revision`, and subsequent waits needed `after_revision` set to the latest observed value. This worked, but the relationship between the top-level revision and individual job output was initially not intuitive.

I learned to retain the latest returned `revision`, then call `jobs` with the existing `job_ids`, `after_revision` equal to that revision, `until: "output"`, and a short timeout. An example showing a complete launch → incremental wait → incremental wait → kill lifecycle would be valuable.

### [DONE] Killing a job does not necessarily produce the program’s signal-handling summary

I killed the first `isoping` process with `jobs_kill`. The captured output stopped, but `isoping` did not print its SIGINT summary. `jobs_kill` is designed for reliable process-tree teardown, not graceful application interruption.

To get final statistics, I launched a second bounded run under `timeout --signal=INT --kill-after=3s 15s ...`. That delivered SIGINT to `isoping`, allowing its handler to exit the loop and print the summary. The wrapper itself returned exit code 124 because the timeout expired, even though the application produced the desired final output.

A separate “send signal” tool, or a `signal` option on `jobs_kill`, could support graceful shutdown before forced teardown. Documentation could also explicitly distinguish “stop/kill the process tree” from “send an application-handled SIGINT.”

### Frame names versus UUIDs

I selected the frame using the name `deb`, while job results identified it by UUID. This worked, but the change in representation can be mildly disorienting when correlating calls and results. Returning both the requested frame alias and resolved UUID would make logs easier to follow.

## What worked well

The clone and package installation jobs were easy to run once sequenced correctly. `jobs` preserved complete logs even when inline output was truncated, and `view(job_id=...)` made it easy to retrieve the final tail. Background execution plus revision-based waiting was a good fit for a program that runs forever. `jobs_kill` reliably tore down the accidental server and the first test client, including their process trees.

The `description` field also provided useful visible progress, and explicit `workdir`, `user`, and `hard_timeout` fields made execution context clear.

## Recommended usage pattern learned

For a workflow like this, I would now proceed sequentially: first list frames, then clone/inspect and wait for completion, then install dependencies and wait, then build and wait. After inspecting the program’s CLI, I would launch the long-running client without a wait or with a short `until: "output"` wait, retain its job ID and latest revision, and poll incrementally with `after_revision`. I would use `view` only after deciding whether I am reading a path or a job log, omitting all fields from the other mode. Finally, I would use an application-level timeout or signal mechanism when graceful summary output matters, and reserve `jobs_kill` for guaranteed teardown.

## Potential API improvements

The highest-value improvements would be a [DONE] discriminated-union or split API for file viewing versus job-log viewing; clearer timeout-result messaging that explicitly says the job is still running; a [DONE] simpler replacement for incremental output revisions; [DONE] graceful signal delivery separate from forced process-tree teardown; and [DONE] warnings against putting dependency chains into concurrent launch arrays.

### [DONE] A killed `go run` job left its compiled child serving traffic

While iterating on the isoping web app, the server was launched as `go run ./cmd/isoping-web`. I later called `jobs_kill` on that job and launched a replacement. The replacement appeared to start successfully, but exited almost immediately with exit code 0 while an HTTP server continued answering on port 8080. Requests showed that the listener was still serving the old 1,024-byte HTML rather than the newly compiled responsive page.

The apparent cause was the process topology created by `go run`: the Go tool builds a temporary executable and starts it as a child. In this case, stopping the tracked job did not result in the temporary compiled child being torn down, despite the API contract saying that `jobs_kill` stops selected background jobs “including child and grandchild processes.” The orphan retained port 8080, so subsequent server attempts could not take over the listener. Their startup output was also misleading because the web program ignored the error returned by `http.ListenAndServe`, allowing bind failure to exit cleanly after printing its URL.

This may be a race in process-tree discovery or teardown rather than a general inability to kill children: a wrapper such as `go run` can fork or exec a generated binary whose identity changes quickly, and the child may be reparented before teardown completes. Regardless, a returned killed state did not correspond to the externally observable process tree being gone.

The reliable workaround was to avoid `go run` for a long-running managed service: first build a stable binary, then launch it directly with `exec`, for example `go build -o .isoping-web ./cmd/isoping-web && exec ./.isoping-web -listen 0.0.0.0:8080`. I also verified the actual served HTML after restart instead of treating startup output as proof that the new process owned the port.

Potential improvements include putting each job in a dedicated cgroup or PID namespace and terminating all members of that group, rather than relying only on a process tree that can change during teardown. After killing, the API could report surviving descendants or processes still holding sockets inherited from the job. Documentation could warn that compiler/runner wrappers such as `go run`, `npm`, and shell launchers are riskier for persistent services and recommend building then execing the final binary. Application examples should also check the error returned by `ListenAndServe`, since a printed startup URL does not prove that binding succeeded.

### [DONE] `create_file` produced a root-owned file that normal jobs could not edit

The report was originally written with `thundersnap_create_file`. That operation runs as root, so `/work/thundersnap/sol-mcp-report.md` was owned by root. Later, a normal `jobs` command running as the default `user` account tried to append to it and failed with `Permission denied`. I had to repeat the append as root.

This behavior is mentioned in the `create_file` documentation (“files intended for user jobs may need ownership adjusted separately”), but it is easy to miss because most repository work is performed as `user`, while the convenient synchronous file-writing tool silently creates an ownership boundary. The failure tends to appear much later, when a formatter, compiler, Git operation, or append attempts to replace or modify the file.

The practical workaround is to run `chown user:user` immediately after `create_file` when writing under a user-owned work tree, or use a `jobs` shell command as `user` to create the file in the first place. An API improvement would be an optional `user` or ownership parameter on `create_file`, defaulting to the frame's normal user for paths under `/work`. At minimum, the result could explicitly report the resulting owner and warn when a root-owned file is created beneath a user-owned directory.
