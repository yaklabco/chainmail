import { describe, expect, test } from 'bun:test';
import { naturalSize, shouldOfferExpand } from './diagram-expand.mjs';

describe('shouldOfferExpand', () => {
  test('offers it when the diagram is shown below 90% of its natural width', () => {
    expect(shouldOfferExpand(1446, 820)).toBe(true);
    expect(shouldOfferExpand(1000, 899)).toBe(true);
  });

  test('does not offer it for a diagram shown at or near full size', () => {
    expect(shouldOfferExpand(1000, 900)).toBe(false);
    expect(shouldOfferExpand(640, 640)).toBe(false);
  });

  test('does not offer it before the diagram has a size', () => {
    expect(shouldOfferExpand(0, 820)).toBe(false);
    expect(shouldOfferExpand(1446, 0)).toBe(false);
    expect(shouldOfferExpand(Number.NaN, 820)).toBe(false);
  });
});

describe('naturalSize', () => {
  test('reads the width and height from the viewBox', () => {
    expect(naturalSize('-8 -8 1446.5 1448')).toEqual({ width: 1446.5, height: 1448 });
    expect(naturalSize('0,0,300,200')).toEqual({ width: 300, height: 200 });
  });

  test('returns zero for a missing or malformed viewBox', () => {
    expect(naturalSize(null)).toEqual({ width: 0, height: 0 });
    expect(naturalSize('0 0 wide')).toEqual({ width: 0, height: 0 });
  });
});
