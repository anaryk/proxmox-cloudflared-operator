import { describe, expect, test } from 'vitest'

import { decode, toUnicode } from './punycode'

const points = (s: string) => String.fromCodePoint(...s.split(' ').map((p) => parseInt(p, 16)))

// The sample strings of RFC 3492, section 7.1, as code points and encoded.
const samples: [string, string, string][] = [
  ['(A) Arabic (Egyptian)', '644 64A 647 645 627 628 62A 643 644 645 648 634 639 631 628 64A 61F', 'egbpdaj6bu4bxfgehfvwxn'],
  ['(B) Chinese (simplified)', '4ED6 4EEC 4E3A 4EC0 4E48 4E0D 8BF4 4E2D 6587', 'ihqwcrb4cv8a8dqg056pqjye'],
  ['(C) Chinese (traditional)', '4ED6 5011 7232 4EC0 9EBD 4E0D 8AAA 4E2D 6587', 'ihqwctvzc91f659drss3x8bo0yb'],
  ['(D) Czech', '50 72 6F 10D 70 72 6F 73 74 11B 6E 65 6D 6C 75 76 ED 10D 65 73 6B 79', 'Proprostnemluvesky-uyb24dma41a'],
  ['(E) Hebrew', '5DC 5DE 5D4 5D4 5DD 5E4 5E9 5D5 5D8 5DC 5D0 5DE 5D3 5D1 5E8 5D9 5DD 5E2 5D1 5E8 5D9 5EA', '4dbcagdahymbxekheh6e0a7fei0b'],
  [
    '(F) Hindi (Devanagari)',
    '92F 939 932 94B 917 939 93F 928 94D 926 940 915 94D 92F 94B 902 928 939 940 902 92C 94B 932 938 915 924 947 939 948 902',
    'i1baa7eci9glrd9b2ae1bj0hfcgg6iyaf8o0a1dig0cd',
  ],
  [
    '(G) Japanese (kanji and hiragana)',
    '306A 305C 307F 3093 306A 65E5 672C 8A9E 3092 8A71 3057 3066 304F 308C 306A 3044 306E 304B',
    'n8jok5ay5dzabd5bym9f0cm5685rrjetr6pdxa',
  ],
  [
    '(H) Korean (Hangul syllables)',
    'C138 ACC4 C758 BAA8 B4E0 C0AC B78C B4E4 C774 D55C AD6D C5B4 B97C C774 D574 D55C B2E4 BA74 C5BC B9C8 B098 C88B C744 AE4C',
    '989aomsvi5e83db1d2a355cv1e0vak1dwrv93d5xbh15a0dt30a5jpsd879ccm6fea98c',
  ],
  [
    '(I) Russian (Cyrillic)',
    '43F 43E 447 435 43C 443 436 435 43E 43D 438 43D 435 433 43E 432 43E 440 44F 442 43F 43E 440 443 441 441 43A 438',
    'b1abfaaepdrnnbgefbaDotcwatmq2g4l',
  ],
  [
    '(J) Spanish',
    '50 6F 72 71 75 E9 6E 6F 70 75 65 64 65 6E 73 69 6D 70 6C 65 6D 65 6E 74 65 68 61 62 6C 61 72 65 6E 45 73 70 61 F1 6F 6C',
    'PorqunopuedensimplementehablarenEspaol-fmd56a',
  ],
  [
    '(K) Vietnamese',
    '54 1EA1 69 73 61 6F 68 1ECD 6B 68 F4 6E 67 74 68 1EC3 63 68 1EC9 6E F3 69 74 69 1EBF 6E 67 56 69 1EC7 74',
    'TisaohkhngthchnitingVit-kjcr8268qyxafd2f1b9g',
  ],
  ['(L) 3<nen>B<gumi><kinpachi><sensei>', '33 5E74 42 7D44 91D1 516B 5148 751F', '3B-ww4c5e180e575a65lsy2b'],
  [
    '(M) <amuro><namie>-with-SUPER-MONKEYS',
    '5B89 5BA4 5948 7F8E 6075 2D 77 69 74 68 2D 53 55 50 45 52 2D 4D 4F 4E 4B 45 59 53',
    '-with-SUPER-MONKEYS-pc58ag80a8qai00g7n9n',
  ],
  [
    '(N) Hello-Another-Way-<sorezore><no><basho>',
    '48 65 6C 6C 6F 2D 41 6E 6F 74 68 65 72 2D 57 61 79 2D 305D 308C 305E 308C 306E 5834 6240',
    'Hello-Another-Way--fc4qua05auwb3674vfr0b',
  ],
  ['(O) <hitotsu><yane><no><shita>2', '3072 3068 3064 5C4B 6839 306E 4E0B 32', '2-u9tlzr9756bt3uc0v'],
  ['(P) Maji<de>Koi<suru>5<byou><mae>', '4D 61 6A 69 3067 4B 6F 69 3059 308B 35 79D2 524D', 'MajiKoi5-783gue6qz075azm5e'],
  ['(Q) <pafii>de<runba>', '30D1 30D5 30A3 30FC 64 65 30EB 30F3 30D0', 'de-jg4avhby1noc0d'],
  ['(R) <sono><supiido><de>', '305D 306E 30B9 30D4 30FC 30C9 3067', 'd9juau41awczczp'],
  ['(S) -> $1.00 <-', '2D 3E 20 24 31 2E 30 30 20 3C 2D', '-> $1.00 <--'],
]

describe('the samples of RFC 3492', () => {
  test.each(samples)('%s', (_, unicode, encoded) => {
    expect(decode(encoded)).toBe(points(unicode))
  })
})

describe('invalid input is no decoding', () => {
  test.each([
    ['a character that is not ASCII', 'bücher-kva'],
    ['a digit that is no digit', 'bcher-k*a'],
    ['a number that ends too soon', 'bcher-kv'],
    ['a number too large', '99999999'],
    ['a code point past the last', 'eo32g'],
    ['a surrogate', 'ib9b'],
  ])('%s', (_, input) => {
    expect(decode(input)).toBeNull()
  })
})

describe('toUnicode', () => {
  test('the labels in punycode, the others as they are', () => {
    expect(toUnicode('www.xn--bcher-kva.example')).toBe('www.bücher.example')
  })

  test('nothing for a name without a label in punycode', () => {
    expect(toUnicode('www.example.com')).toBeNull()
  })

  test('nothing for a label that does not decode', () => {
    expect(toUnicode('xn--bcher-k*a.example')).toBeNull()
  })

  test('nothing for a label that decodes to ASCII', () => {
    expect(toUnicode('xn--abc-.example')).toBeNull()
  })

  test('the prefix in capitals', () => {
    expect(toUnicode('XN--BCHER-KVA.example')).toBe('bücher.example')
  })
})
