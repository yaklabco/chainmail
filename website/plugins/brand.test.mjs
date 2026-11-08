import { describe, expect, test } from 'bun:test';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

/*
 * The wordmark and the favicon are drawn SVG files. These checks keep them
 * independent of fonts, keep the cursor still for visitors who ask for less
 * motion, and keep the old gopher tile and the retired mascot from coming
 * back.
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

    test(`${path} types the name in, one letter at a time`, () => {
      const svg = read(path);
      expect(svg.match(/<path class="letter"/g)).toHaveLength(9);
      // Reduced motion must show every letter, so the letters are visible by
      // default and only an animation hides them.
      expect(svg).toMatch(/@media \(prefers-reduced-motion: reduce\)\s*\{[^}]*\.letter/);
    });

    test(`${path} ends in an underscore cursor`, () => {
      const cursor = read(path).match(/<rect class="cursor"[^>]*height="(\d+)"/);
      expect(cursor).not.toBeNull();
      expect(Number(cursor[1])).toBeLessThan(150);
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

  test('the gopher mascot is retired from both sites and the README', () => {
    const sources = [...files('site/src'), ...files('website/src'), 'website/docusaurus.config.ts', 'README.md'];
    const hits = sources.filter((path) => /mascot|gopher/i.test(read(path)));
    expect(hits).toEqual([]);
    const images = [...files('site/public/img'), ...files('website/static/img')];
    expect(images.filter((path) => /mascot/.test(path))).toEqual([]);
  });
});
