// github-core-clone-01 entrypoint.
//
// The forge itself is a statically linked Go core vendored in this repository (./forge on Linux,
// ./forge.exe on Windows). This Node entrypoint is the lifecycle shim the harness drives: it launches
// the core as a child process with the trial environment inherited, forwards its stdio and signals,
// and exits with the core's exit code. The core binds an ephemeral port, registers it with the suite,
// serves git smart HTTP / REST v3 / GraphQL v4 / MCP from one shared store, long-polls the suite's
// /done endpoint and exits 0 when the trial is finished.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const FORGE_NAME = 'github-core-clone-01';
const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, process.platform === 'win32' ? 'forge.exe' : 'forge');

const core = spawn(binary, [], { stdio: 'inherit', env: process.env });

core.on('error', (err) => {
  process.stderr.write(`[entry] ${FORGE_NAME}: failed to start forge core: ${err}\n`);
  process.exit(1);
});
core.on('exit', (code, signal) => {
  process.stdout.write(`[entry] ${FORGE_NAME}: forge core exited code=${code} signal=${signal}\n`);
  process.exit(code ?? 1);
});
for (const sig of ['SIGTERM', 'SIGINT'] as const) {
  process.on(sig, () => { core.kill(sig); });
}
