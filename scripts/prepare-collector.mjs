import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import { readFile, mkdir, writeFile, chmod } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

const root = new URL('../', import.meta.url);
const require = createRequire(new URL('package.json', root));
const spec = JSON.parse(await readFile(new URL('package.json', root), 'utf8'));
const lock = JSON.parse(await readFile(new URL('package-lock.json', root), 'utf8'));
const goos = process.env.GOOS || ({ win32: 'windows' }[process.platform] || process.platform);
const goarch = process.env.GOARCH || ({ x64: 'amd64' }[process.arch] || process.arch);
const platform = { windows: 'win32', darwin: 'darwin', linux: 'linux' }[goos];
const arch = { amd64: 'x64', arm64: 'arm64' }[goarch];
if (!platform || !arch) throw new Error('No official collector for requested target');
const packageName = `@ccusage/ccusage-${platform}-${arch}`;
const packagePath = require.resolve(`${packageName}/package.json`);
const installed = JSON.parse(await readFile(packagePath, 'utf8'));
const version = spec.dependencies.ccusage;
if (!/^\d+\.\d+\.\d+$/.test(version) || installed.version !== version) {
  throw new Error('Collector version must match the exact dependency pin');
}
const locked = lock.packages[`node_modules/${packageName}`];
if (locked?.version !== version || !locked.integrity) throw new Error('Collector lock entry missing');
const executable = goos === 'windows' ? 'ccusage.exe' : 'ccusage';
const payload = await readFile(require.resolve(`${packageName}/bin/${executable}`));
const upstreamLicense = (await readFile(require.resolve('ccusage/LICENSE'), 'utf8')).replaceAll('\r\n', '\n');
const recordedLicense = (await readFile(new URL('LICENSE-ccusage', root), 'utf8')).replaceAll('\r\n', '\n');
if (upstreamLicense !== recordedLicense) throw new Error('Update LICENSE-ccusage to match the pinned upstream license');
const manifest = {
  version, goos, goarch, executable,
  sha256: createHash('sha256').update(payload).digest('hex'),
  package: packageName, integrity: locked.integrity,
};
const destination = new URL('internal/usage/collectorassets/', root);
await mkdir(destination, { recursive: true });
for (const [name, data] of [
  ['ccusage.bin', payload],
  ['manifest.json', `${JSON.stringify(manifest, null, 2)}\n`],
  ['LICENSE-ccusage', upstreamLicense],
]) {
  const target = new URL(name, destination);
  let existing;
  try { existing = await readFile(target); } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  if (!existing?.equals(Buffer.from(data))) await writeFile(target, data);
}
await chmod(fileURLToPath(new URL('ccusage.bin', destination)), 0o600);
console.log(`Prepared ccusage ${version} for ${goos}/${goarch} (${manifest.sha256})`);
