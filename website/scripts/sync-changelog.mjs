// Copies the repository's CHANGELOG.md into the docs as the Changelog page,
// so the site renders the one changelog the releases are cut from. The
// generated page is gitignored.
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const site = join(dirname(fileURLToPath(import.meta.url)), '..');
const src = readFileSync(join(site, '..', 'CHANGELOG.md'), 'utf8');
const body = src.replace(/^# Changelog\s*\n/, '');
const out = join(site, 'docs', 'reference', 'changelog.md');
mkdirSync(dirname(out), { recursive: true });
writeFileSync(out, `---\ntitle: Changelog\ndescription: Every release of chainmail and what changed in it.\nsidebar_position: 9\n---\n\n${body}`);
