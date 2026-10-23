// End-to-end walk through the admin console, the H5 shop (in a simulated Telegram Mini App)
// and bot commands. Started by run.sh, which provides the app, database and fake Bot API.
const { chromium } = require('playwright');
const crypto = require('crypto');
const fs = require('fs');
const { execSync } = require('child_process');

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:18731', SHOP_BASE = process.env.E2E_SHOP_BASE || BASE, API_BASE = process.env.E2E_API_BASE || BASE,
  OPS_BASE = process.env.E2E_OPS_BASE || BASE, MERCHANT_BASE = process.env.E2E_MERCHANT_BASE || BASE, TG = process.env.E2E_TG || 'http://127.0.0.1:38081', TOKEN = '111:primary';
const SHOTS = process.env.E2E_SHOTS || __dirname + '/.work/shots';
const DB = process.env.E2E_DB_CONTAINER || 'e2e-db';
fs.mkdirSync(SHOTS, { recursive: true });
const problems = [];
const step = (msg) => console.log('▶', msg);
const sql = (q) => execSync(`docker exec ${DB} mariadb -uroot -pe2e e2e -N -e ${JSON.stringify(q)}`).toString().trim();

function initData(user) {
  const v = { auth_date: String(Math.floor(Date.now() / 1000)), query_id: 'e2e', user: JSON.stringify(user) };
  const check = Object.keys(v).sort().map(k => `${k}=${v[k]}`).join('\n');
  const secret = crypto.createHmac('sha256', 'WebAppData').update(TOKEN).digest();
  v.hash = crypto.createHmac('sha256', secret).update(check).digest('hex');
  return new URLSearchParams(v).toString();
}
const IMG = (color, label) => 'data:image/svg+xml;utf8,' + encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" width="600" height="600"><rect width="600" height="600" fill="${color}"/><text x="300" y="320" font-size="64" text-anchor="middle" fill="white" font-family="sans-serif">${label}</text></svg>`);

function watch(page, name) {
  page.on('pageerror', e => problems.push(`[${name}] page error: ${e.message}`));
  page.on('console', m => { if (m.type() === 'error') problems.push(`[${name}] console: ${m.text()}`); });
  page.on('response', async r => { if (r.status() >= 400) { const body = await r.text().catch(() => ''); problems.push(`[${name}] HTTP ${r.status()} ${r.url()} ${body.slice(0, 200)}`); } });
}
async function shot(page, name) { await page.waitForTimeout(400); await page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: false }); }
async function adminApi(page, path, method = 'GET', body) {
  return page.evaluate(async ([path, method, body]) => {
    const r = await fetch(API + '/admin/api/' + path, { credentials: 'include', method, headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'admin-console' }, body: body ? JSON.stringify(body) : undefined });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(path + ': ' + (data.error || r.status));
    return data;
  }, [path, method, body]);
}
async function pick(page, id, value) { await page.click(`#${id}Trigger`); await page.click(`[data-owner="${id}"][data-value="${value}"]`); }
async function nav(page, key) { await page.click(`[data-nav="${key}"]`); await page.waitForTimeout(500); }
// The shop asks for the payment password before paying or exchanging (and, the first time, has
// it set twice). Answers every prompt that appears.
const PIN = '246810';
async function enterPin(page) {
  for (let i = 0; i < 3; i++) {
    await page.waitForTimeout(500);
    if (!(await page.$('#pinInput'))) return;
    await page.fill('#pinInput', PIN); await page.click('#pinOk');
  }
}
async function submitModal(page) { await page.click('#submitModal'); await page.waitForTimeout(600); }
async function toastText(page) { return page.$eval('#toast', e => e.hidden ? '' : e.textContent).catch(() => ''); }

(async () => {
  const browser = await chromium.launch(process.env.CHROME ? { executablePath: process.env.CHROME } : {});

  step('listeners are separated');
  const status = async url => (await fetch(url)).status;
  // Each listener serves only its own part; /healthz is everywhere.
  const checks = [
    [BASE + '/shop/', 404], [BASE + '/admin/login', 404], [BASE + '/admin/api/session', 404], [BASE + '/shop/api/me', 404],
    [SHOP_BASE + '/shop/', 200], [SHOP_BASE + '/shop/api/me', 404], [SHOP_BASE + '/admin/login', 404],
    [API_BASE + '/shop/api/me', 401], [API_BASE + '/admin/api/session', 401], [API_BASE + '/shop/', 404], [API_BASE + '/admin/login', 404],
    [OPS_BASE + '/admin/login', 200], [OPS_BASE + '/admin/', 200], [OPS_BASE + '/admin/api/session', 404], [OPS_BASE + '/shop/', 404],
    [MERCHANT_BASE + '/admin/login', 200], [MERCHANT_BASE + '/admin/api/session', 404], [MERCHANT_BASE + '/telegram/webhook', 404],
  ];
  if (SHOP_BASE !== BASE) {
    for (const [url, want] of checks) { const got = await status(url); if (got !== want) problems.push(`${url} returned ${got}, want ${want}`); }
    for (const b of [BASE, SHOP_BASE, API_BASE, OPS_BASE, MERCHANT_BASE]) if (await status(b + '/healthz') !== 200) problems.push(b + ' /healthz failed');
    const hook = await fetch(BASE + '/telegram/webhook', { method: 'POST', body: '{}' });
    if (hook.status === 404 && (await hook.text()).includes('page not found')) problems.push('webhook route missing on the main port');
    // The API answers CORS only for the configured consoles and shop.
    const pre = async origin => (await fetch(API_BASE + '/admin/login', { method: 'OPTIONS', headers: { Origin: origin, 'Access-Control-Request-Method': 'POST' } })).status;
    if (await pre(OPS_BASE) !== 204 || await pre('http://evil.example') !== 403) problems.push('API CORS allowlist wrong');
  }

  // ---------- Admin ----------
  const actx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  // Uploaded and banner images live on the placeholder host e2e.example; serve them locally.
  await actx.route('https://e2e.example/**', r => r.fulfill({ contentType: 'image/svg+xml', body: decodeURIComponent(IMG('#5264e8', '图片').split(',')[1]) }));
  const admin = await actx.newPage(); watch(admin, 'admin');
  step('admin login');
  await admin.goto(OPS_BASE + '/admin/login');
  await admin.fill('#username', 'admin'); await admin.fill('#password', 'e2e-password-123');
  await Promise.all([admin.waitForURL('**/admin/'), admin.click('#submit')]);
  await admin.waitForTimeout(800); await shot(admin, '01-admin-home');

  step('console upload');
  // A 1x1 PNG through the page's own upload helper (one-time token; main listener when split).
  const uploaded = await admin.evaluate(async () => {
    const png = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='), c => c.charCodeAt(0));
    const body = new FormData(); body.append('file', new Blob([png], { type: 'image/png' }), 'dot.png'); body.append('type', 'image');
    const r = await sendUpload(body); return { status: r.status, data: await r.json() };
  });
  if (uploaded.status !== 200 || !String(uploaded.data.url).includes('/uploads/')) problems.push('upload failed: ' + JSON.stringify(uploaded));

  step('setup menus and merchant');
  await adminApi(admin, 'menus', 'POST', { code: 'food', title: '美食', action_type: 'submenu', placement: 'inline', role: 'user', enabled: true, row_no: 1, sort: 1 });
  const menus = await adminApi(admin, 'menus');
  const parent = menus.find(m => m.code === 'food');
  await adminApi(admin, 'menus', 'POST', { code: 'food_shops', title: '餐厅', action_type: 'merchants', parent_id: parent.id, placement: 'inline', role: 'user', enabled: true, row_no: 1, sort: 1 });
  const child = (await adminApi(admin, 'menus')).find(m => m.code === 'food_shops');
  await adminApi(admin, 'merchants', 'POST', { name: '曼谷中餐馆', menu_id: child.id, address: '曼谷素坤逸 1 号', hours: '10:00–22:00', enabled: true, commission_rate: '0.1', notify_chat_id: 888 });

  step('create category via UI');
  await nav(admin, 'shop_categories');
  await admin.click('#create'); await admin.fill('#scatName', '中餐'); await submitModal(admin);
  await shot(admin, '02-categories');

  step('create products via UI');
  await nav(admin, 'shop_products');
  await admin.click('#create');
  await admin.fill('#spName', '招牌宫保鸡丁');
  await admin.fill('#spSkus', '小份 | 6.5 | 20\n大份 | 9.9 | 10 | 8.8 | 3');
  await admin.fill('#spImages', IMG('#e8590c', '宫保鸡丁'));
  await admin.fill('#spDesc', '花生米酥脆，鸡丁嫩滑。\n微辣。');
  await admin.check('#spPickup');
  await admin.fill('#spGroupSize', '2'); await admin.fill('#spGroupHours', '24'); await admin.fill('#spGroupRate', '0.8');
  await admin.check('#spFeatured');
  await admin.fill('#spLimit', '5');
  const now = new Date(), later = new Date(Date.now() + 86400000);
  const local = d => new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  await admin.fill('#spPromoStart', local(new Date(now.getTime() - 3600000)));
  await admin.fill('#spPromoEnd', local(later));
  await shot(admin, '03-product-form');
  await submitModal(admin);
  const err1 = await toastText(admin); if (err1 && !err1.includes('已保存')) problems.push('product save: ' + err1);
  await admin.click('#create');
  await admin.fill('#spName', '游戏点卡 100 点');
  await pick(admin, 'spKind', 'virtual');
  await admin.fill('#spSkus', '100 点 | 9.5 | 0');
  await admin.fill('#spImages', IMG('#1971c2', '点卡'));
  await submitModal(admin);
  await shot(admin, '04-products');
  const products = await adminApi(admin, 'shop-products');
  const meal = products.find(p => p.name === '招牌宫保鸡丁'), card = products.find(p => p.name.startsWith('游戏点卡'));
  if (!meal || !card) { problems.push('products not created: ' + JSON.stringify(products.map(p => p.name))); }

  step('import virtual codes via UI');
  await admin.click(`[data-action="codes"][data-id="${card.id}"]`);
  await admin.fill('#codeText', 'CARD-AAA-111\nCARD-BBB-222');
  await submitModal(admin);

  step('coupon + banner');
  await nav(admin, 'shop_coupons');
  await admin.click('#create'); await admin.fill('#cpName', '新人满 10 减 2'); await admin.fill('#cpThreshold', '10'); await admin.fill('#cpDiscount', '2');
  await submitModal(admin); await shot(admin, '05-coupons');
  await adminApi(admin, 'shop-banners', 'POST', { title: '开业大酬宾', image_url: 'https://e2e.example/banner.png', link_type: 'product', link_value: String(meal.id), enabled: true, sort: 0 });

  // ---------- Buyer (simulated Telegram Mini App) ----------
  const buyerUser = { id: 9001, first_name: '小明', username: 'xiaoming', language_code: 'zh-hans' };
  const bctx = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true });
  const fakeTG = (data) => `window.Telegram={WebApp:{initData:${JSON.stringify(data)},ready(){},expand(){},close(){},BackButton:{show(){},hide(){},onClick(){}},HapticFeedback:{notificationOccurred(){}},showConfirm(m,cb){window.__confirm=m;cb(true)},openTelegramLink(u){window.__opened=u},openLink(u){window.__opened=u}}};`;
  await bctx.route('https://telegram.org/js/telegram-web-app.js', r => r.fulfill({ contentType: 'application/javascript', body: fakeTG(initData(buyerUser)) }));
  await bctx.route('https://e2e.example/**', r => r.fulfill({ contentType: 'image/svg+xml', body: decodeURIComponent(IMG('#2f9e44', '轮播图').split(',')[1]) }));
  const shopPage = await bctx.newPage(); watch(shopPage, 'shop');

  step('shop home');
  await shopPage.goto(SHOP_BASE + '/shop/#/');
  await shopPage.waitForTimeout(1200); await shot(shopPage, '10-shop-home');

  step('shop home lists menu categories and their merchants');
  if (!(await shopPage.locator('.chip', { hasText: '美食' }).count())) problems.push('menu category tab missing on shop home');
  if (!(await shopPage.locator('.mcard', { hasText: '曼谷中餐馆' }).count())) problems.push('merchant missing on shop home');
  await shopPage.click('.chip:has-text("美食")'); await shopPage.waitForTimeout(800); await shot(shopPage, '10b-shop-category');
  await shopPage.click('.mcard:has-text("曼谷中餐馆")'); await shopPage.waitForTimeout(900); await shot(shopPage, '10c-shop-merchant');
  const merchantText = await shopPage.textContent('#app');
  if (!merchantText.includes('曼谷素坤逸 1 号') || !merchantText.includes('宫保鸡丁')) problems.push('merchant page incomplete: ' + merchantText.slice(0, 120));
  await shopPage.goto(SHOP_BASE + '/shop/#/'); await shopPage.waitForTimeout(800);
  await shopPage.click('.chip:has-text("全部")'); await shopPage.waitForTimeout(800);
  await shopPage.goto(SHOP_BASE + '/shop/#/me'); await shopPage.waitForTimeout(800);
  const uid = sql("select id from telegram_users where telegram_user_id=9001");
  sql(`update wallet_accounts set available=100 where user_id=${uid}`);
  await shopPage.reload(); await shopPage.waitForTimeout(800); await shot(shopPage, '11-shop-me');

  step('admin opens CNY exchange and credits CNY; buyer exchanges CNY to USDT in the shop');
  await nav(admin, 'currencies'); await shot(admin, '11b-admin-currencies');
  const cnyId = sql("select id from wallet_currencies where code='CNY'");
  await admin.click(`[data-action="edit"][data-id="${cnyId}"]`); await admin.waitForTimeout(400);
  await admin.check('#curExchange'); await submitModal(admin);
  if (sql("select exchangeable from wallet_currencies where code='CNY'") !== '1') problems.push('CNY exchange not enabled');
  await nav(admin, 'users');
  await admin.click(`[data-action="balance"][data-id="${uid}"]`); await admin.waitForTimeout(600);
  await pick(admin, 'adjCurrency', 'CNY');
  await admin.fill('#adjAmount', '720'); await admin.fill('#adjRemark', '现金充值'); await shot(admin, '11c-admin-balance');
  await submitModal(admin);
  if (sql(`select available from wallet_accounts where user_id=${uid} and currency='CNY'`) !== '720.000000') problems.push('CNY credit failed');
  await nav(admin, 'deposits'); await shot(admin, '11c2-admin-deposits');
  const depositText = await admin.textContent('#content');
  if (!depositText.includes('后台加款') || !depositText.includes('现金充值') || !depositText.includes('人民币')) problems.push('console credit missing from the deposit page');
  await shopPage.goto(SHOP_BASE + '/shop/#/exchange'); await shopPage.waitForTimeout(900);
  await shopPage.selectOption('#exFrom', 'CNY'); await shopPage.selectOption('#exTo', 'USDT');
  await shopPage.fill('#exAmount', '72'); await shopPage.waitForTimeout(200); await shot(shopPage, '11d-shop-exchange');
  if ((await shopPage.textContent('#exGet')).trim() !== '10.00') problems.push('exchange preview wrong: ' + await shopPage.textContent('#exGet'));
  await shopPage.click('#exSubmit'); await shopPage.waitForTimeout(400);
  if (!(await shopPage.$('#pinInput'))) problems.push('exchange did not ask to set a payment password');
  else await shot(shopPage, '11d2-shop-pin');
  await enterPin(shopPage); await shopPage.waitForTimeout(1000); await shot(shopPage, '11e-shop-exchanged');
  if (sql(`select count(*) from payment_passwords where user_id=${uid}`) !== '1') problems.push('payment password not set from the shop');
  const afterEx = sql(`select group_concat(concat(currency,'=',available) order by currency) from wallet_accounts where user_id=${uid}`);
  if (afterEx !== 'CNY=648.000000,USDT=110.000000') problems.push('exchange balances wrong: ' + afterEx);
  sql(`update wallet_accounts set available=100 where user_id=${uid} and currency='USDT'`);
  await nav(admin, 'exchanges'); await shot(admin, '11f-admin-exchanges');

  step('claim coupon');
  await shopPage.goto(SHOP_BASE + '/shop/#/coupons/center'); await shopPage.waitForTimeout(800);
  await shopPage.click('[data-claim]'); await shopPage.waitForTimeout(600); await shot(shopPage, '12-coupon-center');

  step('product page → cart');
  await shopPage.goto(SHOP_BASE + '/shop/#/'); await shopPage.waitForTimeout(800);
  await shopPage.locator('#grid .tile').filter({ hasText: '宫保鸡丁' }).first().click();
  await shopPage.waitForTimeout(800); await shot(shopPage, '13-product');
  await shopPage.click('[data-sku]:has-text("大份")'); await shopPage.click('#plus');
  await shot(shopPage, '14-product-promo-sku');
  await shopPage.click('#addCart'); await shopPage.waitForTimeout(500);
  await shopPage.click('#shareBtn'); await shopPage.waitForTimeout(500);
  const shared = await shopPage.evaluate(() => window.__opened || '');
  if (!shared.includes('start%3Dp' + meal.id + '_')) problems.push('share link wrong: ' + shared);

  step('cart → checkout (express, new address, coupon)');
  await shopPage.goto(SHOP_BASE + '/shop/#/cart'); await shopPage.waitForTimeout(800); await shot(shopPage, '15-cart');
  await shopPage.click('[data-checkout]'); await shopPage.waitForTimeout(1200); await shot(shopPage, '16-checkout');
  await shopPage.click('text=添加收货地址'); await shopPage.waitForTimeout(600);
  await shopPage.click('text=新增收货地址'); await shopPage.waitForTimeout(500);
  await shopPage.fill('#aName', '小明'); await shopPage.fill('#aPhone', '0812345678'); await shopPage.fill('#aAddr', '曼谷是隆路 99 号');
  await shopPage.click('#aSave'); await shopPage.waitForTimeout(800);
  await shopPage.click('[data-pick]'); await shopPage.waitForTimeout(1200);
  const coupon = await shopPage.$('[data-cp]:not([data-cp$=":0"])'); if (coupon) await coupon.click(); else problems.push('coupon not offered at checkout');
  await shopPage.waitForTimeout(1000); await shot(shopPage, '17-checkout-ready');
  await shopPage.click('#pay'); await enterPin(shopPage); await shopPage.waitForTimeout(1500); await shot(shopPage, '18-order-paid');
  const orderNo = sql("select order_no from shop_orders order by id desc limit 1");
  const pay = sql("select pay_amount from shop_orders order by id desc limit 1");
  console.log('  order', orderNo, 'paid', pay, '(expected 2×8.8 − 2 = 15.6)');
  if (pay !== '15.600000') problems.push('unexpected pay amount ' + pay);

  step('virtual product buy now');
  await shopPage.goto(SHOP_BASE + '/shop/#/product/' + card.id); await shopPage.waitForTimeout(800);
  await shopPage.click('#buyNow'); await shopPage.waitForTimeout(1200);
  await shopPage.click('#pay'); await enterPin(shopPage); await shopPage.waitForTimeout(1500); await shot(shopPage, '19-virtual-delivered');
  const note = await shopPage.textContent('.code').catch(() => '');
  if (!note.includes('CARD-AAA-111')) problems.push('virtual code not shown: ' + note);

  step('admin ships the order');
  await nav(admin, 'shop_orders'); await shot(admin, '20-admin-orders');
  const orderId = sql(`select id from shop_orders where order_no='${orderNo}'`);
  await admin.click(`[data-action="fulfil"][data-id="${orderId}"]`);
  await admin.fill('#fCarrier', '泰国邮政'); await admin.fill('#fTracking', 'TH123456789');
  await submitModal(admin);

  step('buyer confirms, reviews, applies after-sale');
  await shopPage.goto(SHOP_BASE + '/shop/#/order/' + orderId); await shopPage.waitForTimeout(900); await shot(shopPage, '21-order-shipped');
  await shopPage.click('#confirmOrder'); await shopPage.waitForTimeout(1000);
  await shopPage.goto(SHOP_BASE + '/shop/#/review/' + orderId); await shopPage.waitForTimeout(800);
  await shopPage.click('[data-star$=":4"]'); await shopPage.fill('[data-text]', '味道正宗，送得很快！');
  await shot(shopPage, '22-review-form');
  await shopPage.click('#rvSubmit'); await shopPage.waitForTimeout(900);
  await shopPage.goto(SHOP_BASE + '/shop/#/aftersale/' + orderId); await shopPage.waitForTimeout(800);
  await shopPage.fill('#asAmount', '5'); await shopPage.fill('#asReason', '少了一份米饭');
  await shot(shopPage, '23-aftersale-form');
  await shopPage.click('#asSubmit'); await shopPage.waitForTimeout(1000); await shot(shopPage, '24-order-aftersale');

  step('admin refunds after-sale, replies review, settles');
  await nav(admin, 'shop_aftersales'); await shot(admin, '25-admin-aftersales');
  const asId = sql('select id from shop_after_sales order by id desc limit 1');
  await admin.click(`[data-action="asrefund"][data-id="${asId}"]`); await submitModal(admin);
  await nav(admin, 'shop_reviews');
  const rvId = sql('select id from shop_reviews order by id desc limit 1');
  await admin.click(`[data-action="rvreply"][data-id="${rvId}"]`); await admin.fill('#rvText', '感谢支持，米饭已退款！'); await submitModal(admin);
  await shot(admin, '26-admin-reviews');
  await nav(admin, 'shop_settlements'); await shot(admin, '27-admin-settlements');
  await admin.click('[data-settle]'); await submitModal(admin);
  const stId = sql('select id from shop_settlements order by id desc limit 1');
  await admin.click(`[data-paid="${stId}"]`); await admin.fill('#payRef', 'TRX-E2E-001'); await submitModal(admin);
  await shot(admin, '28-admin-settled');
  await nav(admin, 'shop_reports'); await admin.waitForTimeout(600); await shot(admin, '29-admin-report');

  step('buyer sees product rating and order history');
  await shopPage.goto(SHOP_BASE + '/shop/#/product/' + meal.id); await shopPage.waitForTimeout(900);
  await shopPage.evaluate(() => window.scrollTo(0, document.body.scrollHeight)); await shot(shopPage, '30-product-reviews');
  await shopPage.goto(SHOP_BASE + '/shop/#/orders'); await shopPage.waitForTimeout(800); await shot(shopPage, '31-orders');

  step('group buying: start, share, friend joins');
  await shopPage.goto(SHOP_BASE + '/shop/#/product/' + meal.id); await shopPage.waitForTimeout(900);
  await shopPage.click('[data-sku]:has-text("小份")');
  await shot(shopPage, '32-product-group');
  await shopPage.click('#startGroup'); await shopPage.waitForTimeout(1200); await shot(shopPage, '33-group-checkout');
  await shopPage.click('#pay'); await enterPin(shopPage); await shopPage.waitForTimeout(1500); await shot(shopPage, '34-group-waiting');
  if (!/\/group\/\d+/.test(shopPage.url())) problems.push('group checkout did not open the group page: ' + shopPage.url());
  const groupId = shopPage.url().match(/group\/(\d+)/)?.[1];
  const friend = { id: 9003, first_name: '阿强', username: 'aqiang' };
  const fctx = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true });
  await fctx.route('https://telegram.org/js/telegram-web-app.js', r => r.fulfill({ contentType: 'application/javascript', body: fakeTG(initData(friend)) }));
  const fpage = await fctx.newPage(); watch(fpage, 'friend');
  await fpage.goto(SHOP_BASE + '/shop/#/me'); await fpage.waitForTimeout(800);
  const fid = sql('select id from telegram_users where telegram_user_id=9003');
  sql(`update wallet_accounts set available=50 where user_id=${fid}`);
  await fpage.goto(SHOP_BASE + '/shop/#/group/' + groupId); await fpage.waitForTimeout(900); await shot(fpage, '35-friend-group');
  await fpage.click('text=我也要参团'); await fpage.waitForTimeout(900);
  await fpage.click('#joinGroup'); await fpage.waitForTimeout(1200);
  await fpage.click('text=添加收货地址'); await fpage.waitForTimeout(500);
  await fpage.click('text=新增收货地址'); await fpage.waitForTimeout(400);
  await fpage.fill('#aName', '阿强'); await fpage.fill('#aPhone', '0899999999'); await fpage.fill('#aAddr', '清迈古城 8 号');
  await fpage.click('#aSave'); await fpage.waitForTimeout(700); await fpage.click('[data-pick]'); await fpage.waitForTimeout(1200);
  await fpage.click('#pay'); await enterPin(fpage); await fpage.waitForTimeout(1500); await shot(fpage, '36-group-success');
  const gstatus = sql(`select status from shop_groups where id=${groupId}`);
  const gorders = sql(`select group_concat(status) from shop_orders where group_id=${groupId}`);
  console.log('  group', groupId, gstatus, gorders);
  if (gstatus !== 'success' || gorders !== 'paid,paid') problems.push(`group not completed: ${gstatus} ${gorders}`);

  step('points shown to buyer');
  sql(`update shop_orders set completed_at = NOW() - INTERVAL 10 DAY where status='completed'`);
  await shopPage.goto(SHOP_BASE + '/shop/#/me'); await shopPage.waitForTimeout(800);

  step('buyer contacts merchant, admin replies');
  await shopPage.goto(SHOP_BASE + '/shop/#/product/' + meal.id); await shopPage.waitForTimeout(900);
  await shopPage.click('text=联系商家'); await shopPage.waitForTimeout(800);
  await shopPage.fill('#chatText', '请问大份可以少放辣吗？'); await shopPage.click('#chatSend'); await shopPage.waitForTimeout(900);
  await shot(shopPage, '37-chat-buyer');
  await nav(admin, 'shop_messages'); await shot(admin, '38-admin-messages');
  const convId = sql('select id from shop_conversations order by id desc limit 1');
  await admin.click(`[data-action="chat"][data-id="${convId}"]`); await admin.waitForTimeout(600);
  await admin.fill('#chatReply', '可以，下单时备注“少辣”即可。'); await shot(admin, '39-admin-chat');
  await submitModal(admin);
  await shopPage.goto(SHOP_BASE + '/shop/#/chat/' + meal.merchant_id); await shopPage.waitForTimeout(900); await shot(shopPage, '40-chat-replied');
  const replyNotice = (await (await fetch(TG + '/sent')).json()).find(m => m.chat_id === 9001 && /回复了你/.test(m.text || ''));
  if (!replyNotice || !JSON.stringify(replyNotice.reply_markup || {}).includes('#/chat/')) problems.push('buyer not notified of merchant reply');

  step('friend applies to become a merchant, admin approves');
  await fpage.goto(SHOP_BASE + '/shop/#/me'); await fpage.waitForTimeout(800);
  if (!(await fpage.innerText('#app')).includes('商家入驻')) problems.push('buyer does not see the application entry');
  await fpage.goto(SHOP_BASE + '/shop/#/apply'); await fpage.waitForTimeout(800);
  await fpage.fill('#apName', '阿强水果铺'); await fpage.fill('#apContact', '@aqiang'); await fpage.fill('#apCategory', '水果'); await fpage.fill('#apDesc', '每日新鲜进货');
  await shot(fpage, '41-apply-form');
  await fpage.click('#apSubmit'); await fpage.waitForTimeout(900); await shot(fpage, '42-apply-pending');
  await nav(admin, 'shop_applications'); await shot(admin, '43-admin-applications');
  const appId = sql('select id from shop_merchant_applications order by id desc limit 1');
  await admin.click(`[data-action="appapprove"][data-id="${appId}"]`); await admin.waitForTimeout(400);
  await admin.fill('#appRate', '0.08'); await submitModal(admin); await admin.waitForTimeout(800);
  const appRow = sql(`select concat(status,' ',username,' ',merchant_id) from shop_merchant_applications where id=${appId}`);
  console.log('  application', appRow);
  const welcome = (await (await fetch(TG + '/sent')).json()).find(m => m.chat_id === 9003 && /入驻申请已通过/.test(m.text || ''));
  const password = welcome && (welcome.text.match(/初始密码：<code>([^<]+)<\/code>/) || [])[1];
  if (!appRow.startsWith('approved') || !password) problems.push('approval incomplete: ' + appRow + ' password sent=' + !!password);
  if (password) {
    const mctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    const mpage = await mctx.newPage(); watch(mpage, 'merchant');
    await mpage.goto(MERCHANT_BASE + '/admin/login');
    await mpage.fill('#username', appRow.split(' ')[1]); await mpage.fill('#password', password);
    await Promise.all([mpage.waitForURL('**/admin/'), mpage.click('#submit')]);
    await mpage.waitForTimeout(1000); await shot(mpage, '44-merchant-console');
    const navHtml = await mpage.innerHTML('#navigation');
    if (!navHtml.includes('data-nav="shop_products"') || navHtml.includes('data-nav="configs"') || navHtml.includes('data-nav="withdrawals"')) problems.push('merchant console shows wrong pages');
    await mctx.close();
  }
  await fpage.goto(SHOP_BASE + '/shop/#/apply'); await fpage.waitForTimeout(700); await shot(fpage, '45-apply-approved');
  await fpage.goto(SHOP_BASE + '/shop/#/me'); await fpage.waitForTimeout(800); await shot(fpage, '45b-me-merchant');
  if ((await fpage.innerText('#app')).includes('商家入驻')) problems.push('approved merchant still sees the application entry');

  step('batch import products via the console');
  await nav(admin, 'shop_products');
  const exported = await admin.evaluate(async () => (await fetch(dl('shop-products/export'), { credentials: 'include' })).text());
  if (!exported.includes('招牌宫保鸡丁')) problems.push('export missing products');
  const importFile = SHOTS + '/../import.csv';
  fs.writeFileSync(importFile, exported.trimEnd() + '\n,曼谷中餐馆,中餐,批量导入的炒饭,实物,快递,标准,4.5,30,,,0,是,否,0,,\n');
  await admin.click('#importProducts'); await admin.setInputFiles('#importFile', importFile); await admin.waitForTimeout(1200);
  await shot(admin, '46-import-preview');
  const previewText = await admin.textContent('#importResult');
  if (!/新建\s*1/.test(previewText)) problems.push('import preview unexpected: ' + previewText.slice(0, 120));
  await submitModal(admin); await admin.waitForTimeout(600);
  if (sql("select count(*) from shop_products where name='批量导入的炒饭'") !== '1') problems.push('imported product not created');

  step('audit log page');
  await nav(admin, 'audit_logs'); await shot(admin, '47-audit-log');
  if (!(await admin.innerHTML('#content')).includes('登录')) problems.push('audit log shows no logins');

  step('merchant account requests a payout');
  await adminApi(admin, 'accounts', 'POST', { username: 'meal-owner', password: 'owner-pass-123', role: 'merchant', merchant_id: meal.merchant_id, enabled: true });
  const groupOrder = sql(`select id from shop_orders where group_id=${groupId} order by id limit 1`);
  await adminApi(admin, `shop-orders/${groupOrder}/fulfil`, 'POST', { carrier: '泰国邮政', tracking_no: 'TH2' });
  await adminApi(admin, `shop-orders/${groupOrder}/complete`, 'POST', {});
  const octx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const owner = await octx.newPage(); watch(owner, 'owner');
  if (OPS_BASE !== MERCHANT_BASE) {
    // A merchant account cannot sign in to the operator console.
    await owner.goto(OPS_BASE + '/admin/login');
    await owner.fill('#username', 'meal-owner'); await owner.fill('#password', 'owner-pass-123'); await owner.click('#submit');
    await owner.waitForTimeout(600);
    const err = await owner.textContent('#error');
    if (!err.includes('商户后台')) problems.push('merchant login on operator console: ' + err);
    problems.splice(0, problems.length, ...problems.filter(p => !(p.startsWith('[owner]') && /\b403\b/.test(p)))); // the expected refusal
  }
  await owner.goto(MERCHANT_BASE + '/admin/login');
  await owner.fill('#username', 'meal-owner'); await owner.fill('#password', 'owner-pass-123');
  await Promise.all([owner.waitForURL('**/admin/'), owner.click('#submit')]);
  await owner.waitForTimeout(800); await nav(owner, 'shop_settlements'); await shot(owner, '48-owner-settlements');
  await owner.click('[data-payout]'); await owner.fill('#payoutAddr', 'TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7'); await submitModal(owner);
  await owner.waitForTimeout(600); await shot(owner, '49-owner-payout-requested');
  const payout = sql(`select concat(status,' ',payout_address,' ',net) from shop_settlements order by id desc limit 1`);
  console.log('  payout', payout);
  if (!payout.startsWith('pending TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7')) problems.push('payout request not recorded: ' + payout);

  step('merchant owner adds a staff account');
  await nav(owner, 'merchant_staff'); await owner.click('#create');
  await owner.fill('#stUser', 'packer1'); await owner.fill('#stPass', 'packer-pass-123'); await owner.check('#stp_shop_orders');
  await shot(owner, '50-staff-form'); await submitModal(owner);
  await octx.close();
  const sctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const staffPage = await sctx.newPage(); watch(staffPage, 'staff');
  await staffPage.goto(MERCHANT_BASE + '/admin/login');
  await staffPage.fill('#username', 'packer1'); await staffPage.fill('#password', 'packer-pass-123');
  await Promise.all([staffPage.waitForURL('**/admin/'), staffPage.click('#submit')]);
  await staffPage.waitForTimeout(900); await shot(staffPage, '51-staff-console');
  const staffNav = await staffPage.innerHTML('#navigation');
  if (!staffNav.includes('data-nav="shop_orders"') || staffNav.includes('data-nav="shop_settlements"') || staffNav.includes('data-nav="merchant_staff"')) problems.push('staff sees the wrong pages');
  await sctx.close();

  step('every console page loads');
  for (const key of await admin.$$eval('[data-nav]', els => els.map(e => e.dataset.nav))) {
    await nav(admin, key); await admin.waitForTimeout(300);
    const err = await admin.$eval('#error', e => e.hidden ? '' : e.textContent).catch(() => '');
    if (err) problems.push(`page ${key}: ${err}`);
  }

  step('to-do digest and database backup');
  await nav(admin, 'shop_reports'); await admin.click('#rpDigest'); await admin.waitForTimeout(800);
  const digestMsg = (await (await fetch(TG + '/sent')).json()).find(m => m.chat_id === 777 && /今日待办汇总/.test(m.text || ''));
  console.log('  digest to admins:', !!digestMsg);
  await nav(admin, 'configs'); await shot(admin, '52-configs-backup');
  const backupInfo = await admin.evaluate(async () => { const r = await fetch(dl('backup'), { credentials: 'include' }); const b = new Uint8Array(await r.arrayBuffer()); return { status: r.status, size: b.length, magic: b[0] === 0x1f && b[1] === 0x8b, name: r.headers.get('content-disposition') }; });
  console.log('  backup', JSON.stringify(backupInfo));
  if (backupInfo.status !== 200 || !backupInfo.magic || backupInfo.size < 1000) problems.push('backup download failed: ' + JSON.stringify(backupInfo));
  if (sql("select count(*) from admin_audit_logs where method='BACKUP'") !== '1') problems.push('backup not audited');

  step('bot: /orders and share link');
  const msg = (text) => fetch(TG + '/inject', { method: 'POST', body: JSON.stringify({ message: { message_id: 1, date: 1, text, from: { id: 9002, first_name: '小红' }, chat: { id: 9002, type: 'private' } } }) });
  const code = sql('select referral_code from telegram_users where telegram_user_id=9001');
  await msg(`/start p${meal.id}_${code}`);
  await fetch(TG + '/inject', { method: 'POST', body: JSON.stringify({ message: { message_id: 2, date: 1, text: '/orders', from: { id: 9001, first_name: '小明' }, chat: { id: 9001, type: 'private' } } }) });
  await new Promise(r => setTimeout(r, 4000));
  const sent = await (await fetch(TG + '/sent')).json();
  const toFriend = sent.filter(m => m.chat_id === 9002);
  const shareMsg = toFriend.find(m => JSON.stringify(m.reply_markup || {}).includes('#/product/' + meal.id));
  if (!shareMsg) problems.push('no product button sent to the invited friend');
  const referrer = sql('select parent_user_id from telegram_users where telegram_user_id=9002');
  if (referrer !== uid) problems.push('share link did not bind referrer: ' + referrer);
  const ordersMsg = sent.filter(m => m.chat_id === 9001 && /最近的商城订单/.test(m.text || ''));
  if (!ordersMsg.length || !JSON.stringify(ordersMsg[0].reply_markup).includes('#/order/')) problems.push('/orders reply missing or without buttons');
  const staff = sent.filter(m => m.chat_id === 777 || m.chat_id === 888);
  const buyerNotices = sent.filter(m => m.chat_id === 9001 && m.reply_markup && JSON.stringify(m.reply_markup).includes('#/order/'));
  console.log(`  messages: ${sent.length} total, ${staff.length} to staff, ${buyerNotices.length} buyer notices with order buttons`);
  if (!staff.length) problems.push('staff received no notifications');
  fs.writeFileSync(SHOTS + '/../sent.json', JSON.stringify(sent, null, 1));

  step('bot: red packets (group /hb, private wizard, button and password claims, inline share)');
  sql(`update wallet_accounts set available=100 where user_id=${uid} and currency='USDT'`);
  let seq = 1000;
  const inject = u => fetch(TG + '/inject', { method: 'POST', body: JSON.stringify({ _bot: 111, ...u }) });
  const wait = ms => new Promise(r => setTimeout(r, ms));
  const xm = { id: 9001, first_name: '小明', username: 'xiaoming' }, xh = { id: 9002, first_name: '小红' }, passer = { id: 9005, first_name: '路人' };
  const group = { id: -100555, type: 'supergroup' }, pchat = { id: 9001, type: 'private' };
  const say = (from, text, chat) => inject({ message: { message_id: seq++, date: 1, text, from, chat: chat || { id: from.id, type: 'private' } } });
  const tap = (from, data, message) => inject({ callback_query: { id: 'cb' + seq++, from, data, message } });
  const sentNow = async () => (await (await fetch(TG + '/sent')).json());
  const sendPacket = async (type, steps) => {
    await say(xm, '/start hb_-100555'); await wait(900);
    await tap(xm, 'hbw:t:' + type, { message_id: 1, chat: pchat }); await wait(700);
    await tap(xm, 'hbw:c:USDT', { message_id: 1, chat: pchat }); await wait(700);
    for (const st of steps) {
      if (st === 'default-greeting') await tap(xm, 'hbw:g', { message_id: 1, chat: pchat });
      else if (st.startsWith('cover:')) await tap(xm, 'hbw:cv:' + st.slice(6), { message_id: 1, chat: pchat });
      else await say(xm, st);
      await wait(700);
    }
    const confirm = (await sentNow()).filter(m => m.chat_id === 9001 && JSON.stringify(m.reply_markup || {}).includes('hbw:send:')).pop();
    const data = confirm && JSON.stringify(confirm.reply_markup).match(/hbw:send:[0-9a-f]+/)[0];
    if (!data) { problems.push('red packet confirmation not shown for ' + type); return null; }
    await tap(xm, data, { message_id: 1, chat: pchat }); await wait(800);
    await say(xm, '135790'); await wait(800);
    const pinMsg = seq; await say(xm, PIN); await wait(1500);
    const toXm = (await sentNow()).filter(m => m.chat_id === 9001);
    if (!toXm.some(m => /支付密码不正确/.test(m.text || ''))) problems.push('wrong payment password not reported');
    if (!(await sentNow()).some(m => m.method === 'deleteMessage' && m.chat_id === 9001 && m.message_id === pinMsg)) problems.push('payment password message not deleted');
    return sql("select concat(code,' ',type,' ',total,' ',count,' ',chat_id,' ',message_id) from red_packets order by id desc limit 1").split(' ');
  };
  // Added through a startgroup link: "/start@bot hb" introduces the bot instead of opening a wizard.
  await say(xh, '/start@e2e_bot_111 hb', group); await wait(1200);
  if (!(await sentNow()).some(m => m.chat_id === -100555 && /红包机器人已加入本群/.test(m.text || ''))) problems.push('startgroup /start not answered with an introduction');
  // In the group: /start opens the wizard as a group message; only its starter can use it, and
  // answers are replies to it. The wizard message then becomes the packet card.
  await say(xm, '/start@e2e_bot_111', group); await wait(1500);
  const wizard = (await sentNow()).filter(m => m.chat_id === -100555 && m.method === 'sendMessage' && JSON.stringify(m.reply_markup || {}).includes('hbw:t:lucky')).pop();
  let lucky = null;
  if (!wizard) problems.push('group /start did not start the wizard in the group');
  else {
    const wm = { message_id: wizard.mid, chat: group };
    const reply = (text) => inject({ message: { message_id: seq++, date: 1, text, from: xm, chat: group, reply_to_message: { message_id: wizard.mid } } });
    await tap(xm, 'hbw:t:lucky', wm); await wait(700);
    await tap(passer, 'hbw:c:USDT', wm); await wait(700);
    if (!(await sentNow()).some(m => m.method === 'answerCallbackQuery' && m.show_alert && /只有发红包的人/.test(m.text || ''))) problems.push('others could use the group wizard');
    const currencyStep = (await sentNow()).filter(m => m.method === 'editMessageText' && m.chat_id === -100555 && m.message_id === wizard.mid).pop();
    if (!currencyStep || /可用/.test(JSON.stringify(currencyStep.reply_markup))) problems.push('group wizard shows balances or no currency step');
    await tap(xm, 'hbw:c:USDT', wm); await wait(700);
    await say(xm, '聊点别的', group); await wait(500); // not a reply: ignored by the wizard
    for (const st of ['3', '3', '新年快乐']) { await reply(st); await wait(700); }
    const confirm = (await sentNow()).filter(m => m.chat_id === -100555 && m.message_id === wizard.mid && JSON.stringify(m.reply_markup || {}).includes('hbw:send:')).pop();
    const data = confirm && JSON.stringify(confirm.reply_markup).match(/hbw:send:[0-9a-f]+/)[0];
    if (!data) problems.push('group wizard confirmation not shown');
    else {
      await tap(xm, data, wm); await wait(800);
      if (!(await sentNow()).some(m => m.method === 'answerCallbackQuery' && (m.url || '').includes('start=hbpay'))) problems.push('group confirm did not open the private chat for the password');
      await say(xm, '/start hbpay'); await wait(700);
      await say(xm, PIN); await wait(1500);
      lucky = sql("select concat(code,' ',type,' ',total,' ',count,' ',chat_id,' ',message_id) from red_packets order by id desc limit 1").split(' ');
      if (Number(lucky[5]) !== wizard.mid) problems.push('the wizard message did not become the packet card');
    }
    if ((await sentNow()).some(m => m.chat_id === 9001 && /发红包到群/.test(m.text || ''))) problems.push('group /hb still used the private chat');
  }
  if (lucky) {
    console.log('  lucky packet', lucky.join(' '));
    if (lucky[1] !== 'lucky' || lucky[2] !== '3.000000' || lucky[4] !== '-100555' || lucky[5] === '0') problems.push('lucky packet wrong: ' + lucky.join(' '));
    if (!(await sentNow()).some(m => m.chat_id === -100555 && /小明 发了一个拼手气红包/.test(m.text || ''))) problems.push('packet not posted to the group');
    const gm = { message_id: Number(lucky[5]), chat: group };
    await tap(xh, 'hb:' + lucky[0], gm); await tap(passer, 'hb:' + lucky[0], gm); await wait(1200); await tap(xh, 'hb:' + lucky[0], gm); await wait(1500);
    const claims = sql(`select count(*) from red_packet_claims c join red_packets p on p.id=c.packet_id where p.code='${lucky[0]}'`);
    const alerts = (await sentNow()).filter(m => m.method === 'answerCallbackQuery' && m.show_alert).map(m => m.text);
    console.log('  claims', claims, '| alerts', alerts.join(' / '));
    if (claims !== '2' || !alerts.some(t => /抢到/.test(t)) || !alerts.some(t => /已经领过/.test(t))) problems.push('button claims wrong: ' + claims + ' ' + alerts.join('/'));
    if (!(await sentNow()).some(m => m.method === 'editMessageText' && m.chat_id === -100555 && /已领取 2\/3/.test(m.text || ''))) problems.push('group packet card not updated after claims');
    await inject({ inline_query: { id: 'iq' + seq++, from: xm, query: 'hb ' + lucky[0], offset: '' } }); await wait(1200);
    const iq = (await sentNow()).filter(m => m.method === 'answerInlineQuery').pop();
    if (!iq || iq.results.length !== 1 || !/抢红包/.test(JSON.stringify(iq.results[0].reply_markup))) problems.push('inline query did not offer the packet');
    // The last share hides the claim button; a later tap only gets "已领完".
    await tap({ id: 9006, first_name: '阿三' }, 'hb:' + lucky[0], gm); await wait(1200);
    const finished = (await sentNow()).filter(m => m.method === 'editMessageText' && m.chat_id === -100555 && m.message_id === Number(lucky[5])).pop();
    if (!finished || !/已领完/.test(finished.text) || JSON.stringify(finished.reply_markup).includes('hb:' + lucky[0])) problems.push('finished packet still shows the claim button');
    await tap({ id: 9007, first_name: '阿四' }, 'hb:' + lucky[0], gm); await wait(1000);
    if (!(await sentNow()).some(m => m.method === 'answerCallbackQuery' && /已被领完/.test(m.text || ''))) problems.push('tap on a finished packet not answered');
  }
  const pw = await sendPacket('password', ['2', '2', '芝麻开门', 'default-greeting']);
  if (pw) {
    const pwMsgId = seq;
    await say(xh, '芝麻开门', group); await wait(1500);
    // Like a button claim, the result only shows in the card's claim list; no reply message.
    if ((await sentNow()).some(m => m.chat_id === -100555 && /小红 抢到/.test(m.text || ''))) problems.push('password claim in group sent a reply');
    if (!(await sentNow()).some(m => m.method === 'editMessageText' && m.chat_id === -100555 && m.message_id === Number(pw[5]) && /小红/.test(m.text || ''))) problems.push('password claim in group not shown on the card');
    if (!(await sentNow()).some(m => m.method === 'deleteMessage' && m.chat_id === -100555 && m.message_id === pwMsgId)) problems.push('password message not deleted');
    // The claim button of a password packet opens the private chat, which asks for the password.
    if (!(await sentNow()).some(m => m.chat_id === -100555 && JSON.stringify(m.reply_markup || {}).includes('hb:' + pw[0]))) problems.push('password packet has no claim button');
    await tap(passer, 'hb:' + pw[0], { message_id: Number(pw[5]), chat: group }); await wait(1000);
    const opened = (await sentNow()).filter(m => m.method === 'answerCallbackQuery' && (m.url || '').includes('start=hbp_' + pw[0]));
    if (!opened.length) problems.push('password packet button did not open the private chat');
    await say(passer, '/start hbp_' + pw[0]); await wait(800);
    await say(passer, '不对'); await wait(800);
    await say(passer, '芝麻开门'); await wait(1200);
    const toPasser = (await sentNow()).filter(m => m.chat_id === 9005).map(m => m.text || '');
    if (!toPasser.some(t => /请发送口令/.test(t)) || !toPasser.some(t => /口令不对/.test(t)) || !toPasser.some(t => /抢到/.test(t))) problems.push('password claim in private chat failed: ' + toPasser.join(' | '));
    if (sql(`select status from red_packets where code='${pw[0]}'`) !== 'finished') problems.push('password packet not finished after two claims');
    const pwCard = (await sentNow()).filter(m => m.method === 'editMessageText' && m.chat_id === -100555 && m.message_id === Number(pw[5])).pop();
    if (!pwCard || JSON.stringify(pwCard.reply_markup).includes('hb:' + pw[0])) problems.push('finished password packet still shows its button');
  }
  const xmBal = sql(`select available from wallet_accounts where user_id=${uid} and currency='USDT'`);
  if (xmBal !== '95.000000') problems.push('sender not charged 5 USDT for two packets: ' + xmBal);
  // Covers: added in the console, offered after the greeting (an album preview in the private
  // chat), and the card becomes a photo whose caption is edited on claims.
  await nav(admin, 'redpacket_covers'); await admin.click('#create'); await admin.waitForTimeout(400);
  await admin.fill('#rcName', '新年快乐'); await admin.fill('#rcImage', 'https://example.com/cover-newyear.jpg');
  await admin.check('#rcDefault'); await submitModal(admin); await admin.waitForTimeout(500);
  await admin.click('#create'); await admin.waitForTimeout(400);
  await admin.fill('#rcName', '生日快乐'); await admin.fill('#rcImage', 'https://example.com/cover-birthday.png'); await submitModal(admin); await admin.waitForTimeout(500);
  await shot(admin, '55c-admin-redpacket-covers');
  const coverId = sql("select id from red_packet_covers where name='新年快乐' and is_default=1");
  if (!coverId) problems.push('red packet cover not saved as default');
  else {
    const covered = await sendPacket('equal', ['1', '2', 'default-greeting', 'cover:' + coverId]);
    const now = await sentNow();
    const album = now.filter(m => m.method === 'sendMediaGroup' && m.chat_id === 9001).pop();
    if (!album || album.media.length !== 2 || album.media[0].media !== 'https://example.com/cover-newyear.jpg') problems.push('cover choices not previewed as an album');
    if (!now.some(m => m.chat_id === 9001 && /封面：新年快乐/.test(m.text || ''))) problems.push('confirmation does not name the cover');
    if (covered) {
      const posted = now.find(m => m.method === 'sendPhoto' && m.chat_id === -100555 && m.mid === Number(covered[5]));
      if (!posted || posted.photo !== 'https://example.com/cover-newyear.jpg' || !/小明 发了一个普通红包/.test(posted.caption || '')) problems.push('covered packet not posted as a photo');
      const own = now.filter(m => m.method === 'sendPhoto' && m.chat_id === 9001).pop();
      if (!own || own.photo !== 'fid-' + covered[5]) problems.push('private card did not reuse the cover file ID');
      await tap(xh, 'hb:' + covered[0], { message_id: Number(covered[5]), chat: group }); await wait(1200);
      if (!(await sentNow()).some(m => m.method === 'editMessageCaption' && m.chat_id === -100555 && m.message_id === Number(covered[5]) && /小红/.test(m.caption || ''))) problems.push('covered card caption not updated after a claim');
      await inject({ inline_query: { id: 'iq' + seq++, from: xm, query: 'hb ' + covered[0], offset: '' } }); await wait(1200);
      const iq = (await sentNow()).filter(m => m.method === 'answerInlineQuery').pop();
      if (!iq || iq.results[0]?.type !== 'photo' || iq.results[0].photo_file_id !== 'fid-' + covered[5]) problems.push('inline share of a covered packet is not a photo');
    }
  }
  await nav(admin, 'redpackets'); await shot(admin, '55-admin-redpackets');
  if (lucky) {
    await admin.click(`[data-action="claims"][data-id="${sql(`select id from red_packets where code='${lucky[0]}'`)}"]`); await admin.waitForTimeout(600);
    await shot(admin, '55b-admin-redpacket-claims');
    if (!(await admin.textContent('#modal')).includes('USDT-TRC20')) problems.push('red packet claims dialog lacks the currency');
    await admin.click('#closeModal').catch(() => admin.keyboard.press('Escape')); await admin.waitForTimeout(300);
  }

  step('red-packet-only business bot');
  await nav(admin, 'bots'); await admin.click('#create'); await admin.waitForTimeout(400);
  await admin.fill('#name', '红包机器人'); await admin.fill('#token', '222:redpacket'); await pick(admin, 'botPurpose', 'redpacket');
  await submitModal(admin); await admin.waitForTimeout(800); await shot(admin, '56-admin-bots');
  if (sql("select purpose from business_bots where telegram_bot_id=222") !== 'redpacket') problems.push('red packet bot not saved with its purpose');
  await wait(5000); // the business bot poller starts within a few seconds
  await fetch(TG + '/inject', { method: 'POST', body: JSON.stringify({ _bot: 222, message: { message_id: seq++, date: 1, text: '/start', from: xh, chat: { id: 9002, type: 'private' } } }) });
  await fetch(TG + '/inject', { method: 'POST', body: JSON.stringify({ _bot: 222, message: { message_id: seq++, date: 1, text: '烧烤', from: xh, chat: { id: 9002, type: 'private' } } }) });
  await wait(2500);
  const rpBot = (await sentNow()).filter(m => m.bot === 222 && m.chat_id === 9002);
  const homes = rpBot.filter(m => /红包机器人/.test(m.text || ''));
  console.log(`  red packet bot replies: ${rpBot.length}, home menus: ${homes.length}`);
  if (homes.length < 2 || !JSON.stringify(homes[0].reply_markup).includes('hbw:new') || JSON.stringify(homes[0].reply_markup).includes('menu:')) problems.push('red packet bot home wrong: ' + JSON.stringify(rpBot.map(m => m.text)));
  if (rpBot.some(m => /商家/.test(m.text || ''))) problems.push('red packet bot answered with merchant search');
  // A client that sends /start twice in a row gets one menu.
  for (let i = 0; i < 2; i++) await fetch(TG + '/inject', { method: 'POST', body: JSON.stringify({ _bot: 222, message: { message_id: seq++, date: 1, text: '/start', from: passer, chat: { id: 9005, type: 'private' } } }) });
  await wait(2000);
  const passerHomes = (await sentNow()).filter(m => m.bot === 222 && m.chat_id === 9005 && /红包机器人/.test(m.text || '')).length;
  if (passerHomes !== 1) problems.push('double /start answered ' + passerHomes + ' times');
  const groupCmds = (await sentNow()).filter(m => m.method === 'setMyCommands' && m.bot === 222 && m.scope && m.scope.type === 'all_group_chats');
  if (!groupCmds.some(m => JSON.stringify(m.commands).includes('"start"'))) problems.push('/start not registered for groups');

  step('news bot: categories, subscriptions, groups/channels, publishing, RSS and search');
  const nb = u => fetch(TG + '/inject', { method: 'POST', body: JSON.stringify({ _bot: 333, ...u }) });
  await nav(admin, 'news_categories'); await admin.click('#create'); await admin.waitForTimeout(400);
  await admin.fill('#ncName', '本地新闻'); await submitModal(admin); await admin.waitForTimeout(500);
  const newsCat = sql("select id from news_categories where name='本地新闻'");
  await nav(admin, 'bots'); await admin.click('#create'); await admin.waitForTimeout(400);
  await admin.fill('#name', '新闻机器人'); await admin.fill('#token', '333:news'); await pick(admin, 'botPurpose', 'news');
  await submitModal(admin); await admin.waitForTimeout(800);
  await wait(5000);
  const pmsg = { message_id: 1, chat: { id: 9001, type: 'private' } };
  await nb({ message: { message_id: seq++, date: 1, text: '/start', from: xm, chat: { id: 9001, type: 'private' } } }); await wait(800);
  await nb({ callback_query: { id: 'cb' + seq++, from: xm, data: 'nw:s', message: pmsg } }); await wait(700);
  await nb({ callback_query: { id: 'cb' + seq++, from: xm, data: 'nw:t:0', message: pmsg } }); await wait(900);
  if (sql("select count(*) from news_subscriptions where telegram_user_id=9001 and category_id=0") !== '1') problems.push('news subscription not saved');
  await nb({ my_chat_member: { chat: { id: -100777, type: 'supergroup', title: '新闻推送群' }, from: xm, date: 1, new_chat_member: { status: 'member', user: { id: 333, is_bot: true, first_name: 'news' } } } });
  await nb({ my_chat_member: { chat: { id: -100888, type: 'channel', title: '快讯频道', username: 'flashchan' }, from: xm, date: 1, new_chat_member: { status: 'administrator', user: { id: 333, is_bot: true, first_name: 'news' } } } });
  await wait(1500);
  await nav(admin, 'news_chats');
  const groupRow = sql("select id from news_chats where chat_id=-100777"), chanRow = sql("select id from news_chats where chat_id=-100888");
  if (!groupRow || !chanRow) problems.push('news chats not recorded');
  else {
    await admin.click(`[data-action="edit"][data-id="${groupRow}"]`); await admin.waitForTimeout(400);
    await admin.check('#nchPush'); await submitModal(admin); await admin.waitForTimeout(500);
    await admin.click(`[data-action="edit"][data-id="${chanRow}"]`); await admin.waitForTimeout(400);
    await admin.check('#nchSource'); await admin.check('#nchAuto'); await submitModal(admin); await admin.waitForTimeout(500);
    await shot(admin, '57-news-chats');
    if (sql("select concat(push_enabled,source_enabled,auto_publish) from news_chats where chat_id=-100888") !== '011' || sql("select push_enabled from news_chats where chat_id=-100777") !== '1') problems.push('news chat settings not saved');
  }
  await nav(admin, 'news'); await admin.click('#create'); await admin.waitForTimeout(400);
  await admin.fill('#naTitle', '曼谷今日要闻'); await admin.fill('#naContent', '轻轨延长线今天开通。'); await admin.fill('#naLink', 'https://example.com/news/1');
  await shot(admin, '58-news-form'); await submitModal(admin); await admin.waitForTimeout(600);
  await nb({ channel_post: { message_id: 5, date: 1, chat: { id: -100888, type: 'channel', title: '快讯频道', username: 'flashchan' }, text: '频道快讯：泰铢走强\n今日汇率上涨' } });
  await wait(2000);
  if (sql("select status from news_articles where title='频道快讯：泰铢走强'") !== 'published') problems.push('channel post not published as news');
  await wait(26000); // the worker queues the channel article on its next round; delivery polls every 5 s
  const newsSent = (await sentNow()).filter(m => m.bot === 333);
  const toUser = newsSent.filter(m => m.chat_id === 9001), toGroup = newsSent.filter(m => m.chat_id === -100777);
  console.log(`  news messages: ${toUser.length} to the subscriber, ${toGroup.length} to the group, ${newsSent.filter(m => m.method === 'copyMessage').length} copied`);
  if (!toUser.some(m => /曼谷今日要闻/.test(m.text || '')) || !toGroup.some(m => /曼谷今日要闻/.test(m.text || ''))) problems.push('published article not delivered to subscriber and group');
  if (!newsSent.some(m => m.method === 'copyMessage' && m.chat_id === 9001 && m.from_chat_id === -100888)) problems.push('channel post not copied to the subscriber');
  if (newsSent.some(m => m.chat_id === -100888)) problems.push('source channel received news');
  await shot(admin, '59-news-list');
  await nav(admin, 'news_feeds'); await admin.click('#create'); await admin.waitForTimeout(400);
  await admin.fill('#nfName', 'E2E 订阅源'); await admin.fill('#nfURL', TG + '/rss.xml'); await submitModal(admin); await admin.waitForTimeout(600);
  const feedId = sql("select id from news_feeds order by id desc limit 1");
  await admin.click(`[data-action="fetch"][data-id="${feedId}"]`); await admin.waitForTimeout(1200); await shot(admin, '60-news-feeds');
  if (sql("select count(*) from news_articles where source='rss' and status='published' and push=0") !== '2') problems.push('RSS first fetch not archived');
  await nb({ message: { message_id: seq++, date: 1, text: '轻轨', from: xm, chat: { id: 9001, type: 'private' } } }); await wait(1500);
  const searchReply = (await sentNow()).filter(m => m.bot === 333 && m.chat_id === 9001 && /搜索「轻轨」/.test(m.text || '')).pop();
  if (!searchReply || !JSON.stringify(searchReply.reply_markup).includes('nw:a:')) problems.push('news search returned nothing');

  await browser.close();
  console.log('\nPROBLEMS (' + problems.length + '):\n' + problems.map(p => ' - ' + p).join('\n'));
  console.log('Screenshots: ' + SHOTS);
  process.exit(problems.length ? 1 : 0);
})().catch(e => { console.error('FATAL', e); console.log('\nPROBLEMS so far:\n' + problems.join('\n')); process.exit(1); });
