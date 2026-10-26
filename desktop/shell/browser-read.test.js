'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const { fakeBrowser } = require('./browser-fake-page.test-helper');
const { browserRead, sliceUtf8, validateReadParams } = require('./browser-read');
const { browserSnapshotForParams } = require('./browser-snapshot-view');

function productPage({ h, head, body }) {
  head.append(h('script', { type: 'application/ld+json' }, JSON.stringify({
    '@context': 'https://schema.org', '@type': 'Product', name: 'Trail Runner 3',
    offers: { '@type': 'Offer', price: '89.99', priceCurrency: 'USD', availability: 'https://schema.org/InStock' },
  })));
  head.append(h('meta', { property: 'og:title', content: 'Trail Runner 3' }));
  head.append(h('meta', { name: 'description', content: 'A light trail shoe' }));
  head.append(h('link', { rel: 'canonical', href: '/p/trail-runner-3' }));
  body.append(
    h('header', {}, h('nav', {}, h('a', { href: '/' }, 'Home'), ' ', h('a', { href: '/sale' }, 'Sale'))),
    h('main', {},
      h('h1', {}, 'Trail Runner 3'),
      h('p', {}, 'Light and ', h('strong', {}, 'grippy'), '. See the ', h('a', { href: 'size-guide' }, 'size guide'), '.'),
      h('img', { src: '/img/tr3.jpg', alt: 'Trail Runner 3 in blue', $width: 400, $height: 300 }),
      h('img', { src: '/pixel.gif', $width: 1, $height: 1 }),
      h('h2', {}, 'Specs'),
      h('table', {},
        h('thead', {}, h('tr', {}, h('th', {}, 'Size'), h('th', {}, 'Stock'))),
        h('tbody', {}, h('tr', {}, h('td', {}, '42'), h('td', {}, 'In stock')), h('tr', {}, h('td', {}, '43'), h('td', {}, 'Low | 2 left')))),
      h('ul', {}, h('li', {}, 'Vibram sole'), h('li', {}, 'Recycled upper', h('ul', {}, h('li', {}, '60% polyester')))),
      h('form', { action: '/cart/add', method: 'post' },
        h('select', { 'aria-label': 'Size' }, h('option', {}, '42'), h('option', { $selected: true }, '43')),
        h('input', { name: 'qty', 'aria-label': 'Quantity', type: 'number', $value: '2' }),
        h('input', { type: 'password', name: 'pin', 'aria-label': 'Gift card PIN', $value: 'PIN-SECRET-77' }),
        h('input', { name: 'session_token', 'aria-label': 'Session', $value: 'TOKEN-SECRET-88' }),
        h('textarea', { 'aria-label': 'Note', $value: 'call me, api_key=NOTE-SECRET-99 thanks' }),
        h('button', { type: 'submit' }, 'Add to cart'),
      ),
      h('div', { $style: { display: 'none' } }, 'HIDDEN PROMO TEXT'),
      h('p', {}, 'Was ', h('s', {}, '$119.99'), ' now $89.99 ', h('button', { 'aria-label': 'Add to wishlist' }, '♡')),
      h('p', {}, h('a', { href: '/p/full', title: 'Full Moon over "Noah’s" Ark' }, 'Full Moon over ...'), ' ', h('a', { href: '/p/same', title: 'Same text' }, 'Same text')),
      h('p', {}, 'Support: ', h('a', { href: 'https://help:hunter2@support.example/chat' }, 'chat with us')),
    ),
    h('footer', {}, h('p', {}, 'Copyright Trail Co')),
  );
}

function readPage(fake, params = {}) {
  return browserRead(fake.manager, fake.entry, params);
}

test('a product page reads as clean markdown with absolute links, tables, fields and structured data first', async () => {
  const fake = fakeBrowser({ url: 'https://shop.example/p/tr3' });
  fake.loadRoot('https://shop.example/p/tr3', productPage);
  const result = await readPage(fake);
  const md = result.content;
  assert.equal(result.complete, true);
  assert.equal(result.next_offset, null);
  assert.equal(result.main_region_first, true);
  assert.equal(result.total_bytes, Buffer.byteLength(md));
  // Structured data comes first, parsed and minified.
  assert.ok(md.startsWith('[Structured data: JSON-LD Product]\n```json\n{"@context":"https://schema.org","@type":"Product"'), md.slice(0, 200));
  assert.match(md, /"price":"89\.99"/);
  assert.match(md, /- og:title: Trail Runner 3/);
  assert.match(md, /- canonical: https:\/\/shop\.example\/p\/trail-runner-3/);
  // Main content before the page chrome.
  assert.ok(md.indexOf('# Trail Runner 3') < md.indexOf('[Home](https://shop.example/)'));
  assert.match(md, /\n# Trail Runner 3\n\nLight and grippy\. See the \[size guide\]\(https:\/\/shop\.example\/p\/size-guide\)\.\n/);
  assert.match(md, /!\[Trail Runner 3 in blue\]\(https:\/\/shop\.example\/img\/tr3\.jpg\)/);
  assert.doesNotMatch(md, /pixel\.gif/, 'tracking pixels are dropped');
  assert.match(md, /\| Size \| Stock \|\n\| --- \| --- \|\n\| 42 \| In stock \|\n\| 43 \| Low \\\| 2 left \|/);
  assert.match(md, /- Vibram sole\n- Recycled upper\n  - 60% polyester/);
  assert.match(md, /\[Form: POST https:\/\/shop\.example\/cart\/add\]/);
  assert.match(md, /\[Select "Size" = "43"; options: 42 \| 43\]/);
  assert.match(md, /\[Input "Quantity" \(number\) = "2"\]/);
  assert.match(md, /\[Password field "Gift card PIN" = \[redacted\]\]/);
  assert.match(md, /\[Input "Session" = \[redacted\]\]/);
  assert.match(md, /\[Button: Add to cart\]/);
  assert.match(md, /\[Page content outside the main region/);
  assert.match(md, /Copyright Trail Co/);
  // Redaction: secret fields, key assignments and URL credentials.
  assert.match(md, /Was ~~\$119\.99~~ now \$89\.99 \[Button: Add to wishlist\]/);
  assert.match(md, /\[Full Moon over \.\.\.\]\(https:\/\/shop\.example\/p\/full "Full Moon over \\"Noah’s\\" Ark"\) \[Same text\]\(https:\/\/shop\.example\/p\/same\)/);
  for (const secret of ['PIN-SECRET-77', 'TOKEN-SECRET-88', 'NOTE-SECRET-99', 'hunter2']) assert.doesNotMatch(md, new RegExp(secret));
  assert.match(md, /api_key=\[redacted\]/);
  assert.match(md, /\[chat with us\]\(https:\/\/\[redacted\]@support\.example\/chat\)/);
  assert.doesNotMatch(md, /HIDDEN PROMO TEXT/);
});

test('a long article is read completely across pages that each stay within max_bytes', async () => {
  const fake = fakeBrowser({ url: 'https://news.example/a' });
  fake.loadRoot('https://news.example/a', ({ h, body }) => {
    const article = h('article', {}, h('h1', {}, 'A long story'));
    for (let index = 1; index <= 600; index += 1) {
      article.append(h('p', {}, `Paragraph ${index}: naïve café résumé — ${'texto largo '.repeat(8)}`, h('a', { href: `/n/${index}` }, `link ${index}`)));
    }
    body.append(article);
  });
  let evaluations = 0;
  const original = fake.webContents.executeJavaScript;
  fake.webContents.executeJavaScript = async (script) => { if (script.includes('pageReadDocument')) evaluations += 1; return original(script); };
  const pages = [];
  let offset = 0;
  for (let guard = 0; guard < 100; guard += 1) {
    const page = await readPage(fake, { offset, maxBytes: 4096 });
    assert.ok(page.bytes_returned <= 4096);
    assert.equal(Buffer.byteLength(page.content), page.bytes_returned);
    assert.doesNotMatch(page.content, /�/, 'pages never split a character');
    pages.push(page);
    if (page.complete) break;
    assert.equal(page.next_offset, page.offset + page.bytes_returned);
    assert.match(page.next, new RegExp(`offset ${page.next_offset}`));
    offset = page.next_offset;
  }
  const last = pages[pages.length - 1];
  assert.equal(last.complete, true);
  const whole = pages.map((page) => page.content).join('');
  assert.equal(Buffer.byteLength(whole), pages[0].total_bytes);
  for (const index of [1, 300, 600]) assert.match(whole, new RegExp(`Paragraph ${index}: naïve café résumé`));
  assert.match(whole, /\[link 600\]\(https:\/\/news\.example\/n\/600\)/);
  assert.ok(pages.length > 10);
  assert.equal(evaluations, 1, 'continuations come from the same extraction');
  // Pages end on line boundaries when one is near.
  assert.ok(pages.slice(0, -1).every((page) => page.content.endsWith('\n')));

  // An interaction replaces the cached read; the continuation says so.
  fake.entry.interactionGeneration += 1;
  const after = await readPage(fake, { offset: pages[1].offset, maxBytes: 4096 });
  assert.match(after.note, /new read/);
  assert.equal(evaluations, 2);
});

test('frames are read through their own contexts and placed where the iframe sits', async () => {
  const fake = fakeBrowser({ url: 'https://shop.example/listing' });
  const root = fake.loadRoot('https://shop.example/listing', ({ h, body }) => {
    body.append(
      h('h1', {}, 'Listing'),
      h('p', {}, 'Before the reviews.'),
      h('iframe', { src: 'https://reviews.example/widget', title: 'Reviews', id: 'reviews' }),
      h('p', {}, 'Between frames.'),
      h('iframe', { src: '/same/frame', id: 'same' }),
      h('iframe', { src: 'https://ads.example/hidden', id: 'hidden-ad', $style: { display: 'none' } }),
      h('p', {}, 'After frames.'),
    );
  });
  const owner = (id) => root.dom.body.children.find((node) => node.getAttribute('id') === id);
  fake.setFrame({ id: 'reviews', parentId: 'root', frameUrl: 'https://reviews.example/widget', kind: 'oopif', owner: owner('reviews'),
    build: ({ h, body }) => body.append(h('h2', {}, 'Reviews'), h('p', {}, 'Five stars, ', h('a', { href: '/r/1' }, 'read review'))) });
  fake.setFrame({ id: 'same', parentId: 'root', frameUrl: 'https://shop.example/same/frame', kind: 'same-process', owner: owner('same'),
    build: ({ h, body }) => body.append(h('p', {}, 'Same-site frame text')) });
  fake.setFrame({ id: 'hidden-ad', parentId: 'root', frameUrl: 'https://ads.example/hidden', kind: 'oopif', owner: owner('hidden-ad'),
    build: ({ h, body }) => body.append(h('p', {}, 'AD TEXT')) });
  const result = await readPage(fake);
  const md = result.content;
  const order = ['Before the reviews.', '[Frame: https://reviews.example/widget]', '## Reviews',
    '[read review](https://reviews.example/r/1)', '[End of frame]', 'Between frames.', 'Same-site frame text', 'After frames.'];
  let at = -1;
  for (const piece of order) {
    const next = md.indexOf(piece, at + 1);
    assert.ok(next > at, `${piece} in order:\n${md}`);
    at = next;
  }
  assert.doesNotMatch(md, /AD TEXT/, 'a frame the page hides is not read into the content');
  assert.equal(result.frames.read, 2);

  // A frame that stops answering is bounded and reported; the rest is read.
  fake.frames.get('reviews').hang = true;
  fake.manager.readChildFramesBudgetMs = 150;
  fake.entry.interactionGeneration += 1;
  const bounded = await readPage(fake);
  assert.match(bounded.content, /\[Frame: https:\/\/reviews\.example\/widget \(not readable: frame_read_timed_out\)\]/);
  assert.match(bounded.content, /After frames\./);
  assert.equal(bounded.frames.unavailable[0].reason, 'frame_read_timed_out');

  // include_frames:false keeps only the frame placeholders.
  fake.frames.get('reviews').hang = false;
  const noFrames = await readPage(fake, { includeFrames: false });
  assert.doesNotMatch(noFrames.content, /Five stars|Same-site frame text/);
  assert.match(noFrames.content, /\[Frame: Reviews https:\/\/reviews\.example\/widget\]/);
});

test('reads can be scoped to selector matches, a snapshot element or a snapshot frame', async () => {
  const fake = fakeBrowser({ url: 'https://shop.example/s' });
  const root = fake.loadRoot('https://shop.example/s', ({ h, body }) => {
    body.append(h('h1', {}, 'Results'));
    for (let index = 1; index <= 3; index += 1) {
      body.append(h('div', { class: 'card', id: `card-${index}` }, h('a', { href: `/itm/${index}` }, `Card ${index}`), ` $${index}0`));
    }
    body.append(h('iframe', { src: 'https://maps.example/embed', id: 'map' }));
  });
  fake.setFrame({ id: 'map', parentId: 'root', frameUrl: 'https://maps.example/embed', kind: 'oopif',
    owner: root.dom.body.children.find((node) => node.getAttribute('id') === 'map'),
    build: ({ h, body }) => body.append(h('p', {}, 'Map of stores'), h('a', { href: '/store/9' }, 'Store 9')) });

  const cards = await readPage(fake, { selector: '.card' });
  assert.equal(cards.selector_matches, 3);
  assert.equal(cards.scope.kind, 'selector');
  assert.match(cards.content, /^\[Card 1\]\(https:\/\/shop\.example\/itm\/1\) \$10\n\n\[Card 2\]/);
  assert.doesNotMatch(cards.content, /Results|Map of stores/);

  const missing = await readPage(fake, { selector: '.nothing' });
  assert.equal(missing.status, 'missing');

  const compact = await browserSnapshotForParams(fake.manager, fake.entry, { format: 'compact' });
  const cardRef = compact.elements.split('\n').find((line) => line.includes('"Card 2"')).split(' ')[0];
  const element = await readPage(fake, { elementRef: cardRef, snapshotId: compact.snapshot_id });
  assert.equal(element.content, '[Card 2](https://shop.example/itm/2)');
  assert.match(compact.frames, /^f1 frame url=https:\/\/maps\.example\/embed access=owned_oopif/);
  const frame = await readPage(fake, { frameRef: 'f1', snapshotId: compact.snapshot_id });
  assert.equal(frame.scope.kind, 'frame');
  assert.equal(frame.content, 'Map of stores\n\n[Store 9](https://maps.example/store/9)');
  const inFrame = await readPage(fake, { frameRef: 'f1', snapshotId: compact.snapshot_id, selector: 'a' });
  assert.equal(inFrame.content, '[Store 9](https://maps.example/store/9)');

  fake.navigate('https://shop.example/other', ({ h, body }) => body.append(h('p', {}, 'Other')));
  const stale = await readPage(fake, { frameRef: 'f1', snapshotId: compact.snapshot_id });
  assert.equal(stale.status, 'stale');
  const staleElement = await readPage(fake, { elementRef: cardRef, snapshotId: compact.snapshot_id });
  assert.equal(staleElement.status, 'stale');
});

test('large structured data moves after the page content with a pointer that names its offset', async () => {
  const fake = fakeBrowser({ url: 'https://shop.example/big' });
  fake.loadRoot('https://shop.example/big', ({ h, head, body }) => {
    const variants = Array.from({ length: 200 }, (_value, index) => ({ '@type': 'Offer', sku: `SKU-${index}`, price: `${index}.00` }));
    head.append(h('script', { type: 'application/ld+json' }, JSON.stringify({ '@type': 'Product', name: 'Big', offers: variants })));
    body.append(h('h1', {}, 'Big product'), h('p', {}, 'Body text.'));
  });
  const result = await readPage(fake);
  const pointer = /\[Structured data \(\d+ KiB\) follows the page content, from offset (\d+)\]/.exec(result.content);
  assert.ok(pointer, result.content.slice(0, 300));
  const at = Number(pointer[1]);
  const tail = Buffer.from(result.content).subarray(at).toString('utf8');
  assert.ok(tail.startsWith('[Structured data: JSON-LD Product]'), tail.slice(0, 80));
  assert.ok(result.content.indexOf('# Big product') < at);
});

test('read arguments are validated and pages slice on character boundaries', () => {
  assert.throws(() => validateReadParams({ maxBytes: 100 }), /max_bytes/);
  assert.throws(() => validateReadParams({ offset: -1 }), /offset/);
  assert.throws(() => validateReadParams({ elementRef: 'e1' }), /snapshot_id/);
  assert.throws(() => validateReadParams({ snapshotId: 's' }), /only used with/);
  assert.throws(() => validateReadParams({ elementRef: 'e1', frameRef: 'f1', snapshotId: 's' }), /not both/);
  assert.deepEqual(validateReadParams({}), { offset: 0, maxBytes: 32768, includeFrames: true });
  const buffer = Buffer.from('ab€cd\nef€gh');
  const first = sliceUtf8(buffer, 0, 4);
  assert.equal(first.content, 'ab');
  const second = sliceUtf8(buffer, 3, 6);
  assert.equal(second.start, 5, 'an offset inside a character moves to the next character');
  assert.equal(second.content, 'cd\nef', 'the end backs off to a character boundary');
  const third = sliceUtf8(Buffer.from('line one\nline two continues'), 0, 12);
  assert.equal(third.content, 'line one\n', 'a newline in the second half of the window ends the page');
});

test('a label that wraps its select names the field without the option texts', async () => {
  const fake = fakeBrowser({ url: 'https://shop.example/f' });
  fake.loadRoot('https://shop.example/f', ({ h, body }) => {
    const select = h('select', { name: 'plan' }, h('option', {}, 'Free'), h('option', { $selected: true }, 'Pro'));
    const label = h('label', {}, 'Plan ', select);
    select.labels = [label];
    body.append(h('form', { action: '/save' }, label));
  });
  const result = await readPage(fake);
  assert.match(result.content, /Plan \[Select "Plan" = "Pro"; options: Free \| Pro\]/);
});
