import { describe, expect, test } from 'bun:test';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

/*
 * The wordmark and the favicon are drawn SVG files. These checks keep them
 * independent of fonts, keep the cursor still for visitors who ask for less
 * motion, and keep the old gopher tile from coming back.
 */

const repo = join(import.meta.dir, '..', '..');
const read = (path) => readFileSync(join(repo, path), 'utf8');

const wordmarks = {
  'website/static/img/wordmark-dark.svg': ['#f3f1ea', '#d4a84b'],
  'website/static/img/wordmark-light.svg': ['#1b1f27', '#8a6516'],
  'site/public/img/wordmark-dark.svg': ['#f3f1ea', '#d4a84b'],
};
const favicons = ['website/static/img/favicon.svg', 'site/public/img/favicon.svg'];

describe('wordmark', () => {
  for (const [path, colours] of Object.entries(wordmarks)) {
    test(`${path} draws its letters as paths`, () => {
      const svg = read(path);
      expect(svg).not.toContain('<text');
      expect(svg).toContain('<path');
    });

    test(`${path} blinks only when motion is allowed`, () => {
      const svg = read(path);
      expect(svg).toContain('steps(1');
      expect(svg).toMatch(/@media \(prefers-reduced-motion: reduce\)\s*\{[^}]*animation:\s*none/);
    });

    test(`${path} uses its colourway`, () => {
      const svg = read(path).toLowerCase();
      for (const colour of colours) expect(svg).toContain(colour);
    });
  }
});

describe('favicon', () => {
  for (const path of favicons) {
    test(`${path} is the rings on the navy tile`, () => {
      const svg = read(path).toLowerCase();
      expect(svg).not.toContain('<text');
      expect(svg).toContain('#090d16');
      expect(svg).toContain('#d4a84b');
    });
  }
});

describe('old logo', () => {
  const files = (dir) =>
    readdirSync(join(repo, dir)).flatMap((name) => {
      const path = join(dir, name);
      return statSync(join(repo, path)).isDirectory() ? files(path) : [path];
    });

  test('nothing references the gopher tile', () => {
    const sources = [...files('site/src'), ...files('website/src'), 'website/docusaurus.config.ts'];
    const hits = sources.filter((path) => read(path).includes('logo-tile'));
    expect(hits).toEqual([]);
  });
});
