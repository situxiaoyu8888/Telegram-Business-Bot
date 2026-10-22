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
  await shopPage.goto(SHOP_BASE + '/shop/#/exchange'); await shopPage.waitForTimeout(900);
  await shopPage.selectOption('#exFrom', 'CNY'); await shopPage.selectOption('#exTo', 'USDT');
  await shopPage.fill('#exAmount', '72'); await shopPage.waitForTimeout(200); await shot(shopPage, '11d-shop-exchange');
  if ((await shopPage.textContent('#exGet')).trim() !== '10.00') problems.push('exchange preview wrong: ' + await shopPage.textContent('#exGet'));
  await shopPage.click('#exSubmit'); await shopPage.waitForTimeout(1000); await shot(shopPage, '11e-shop-exchanged');
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
  await shopPage.click('#pay'); await shopPage.waitForTimeout(1500); await shot(shopPage, '18-order-paid');
  const orderNo = sql("select order_no from shop_orders order by id desc limit 1");
  const pay = sql("select pay_amount from shop_orders order by id desc limit 1");
  console.log('  order', orderNo, 'paid', pay, '(expected 2×8.8 − 2 = 15.6)');
  if (pay !== '15.600000') problems.push('unexpected pay amount ' + pay);

  step('virtual product buy now');
  await shopPage.goto(SHOP_BASE + '/shop/#/product/' + card.id); await shopPage.waitForTimeout(800);
  await shopPage.click('#buyNow'); await shopPage.waitForTimeout(1200);
  await shopPage.click('#pay'); await shopPage.waitForTimeout(1500); await shot(shopPage, '19-virtual-delivered');
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
  await shopPage.click('#pay'); await shopPage.waitForTimeout(1500); await shot(shopPage, '34-group-waiting');
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
  await fpage.click('#pay'); await fpage.waitForTimeout(1500); await shot(fpage, '36-group-success');
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
    for (const st of steps) { if (st === 'default-greeting') await tap(xm, 'hbw:g', { message_id: 1, chat: pchat }); else await say(xm, st); await wait(700); }
    const confirm = (await sentNow()).filter(m => m.chat_id === 9001 && JSON.stringify(m.reply_markup || {}).includes('hbw:send:')).pop();
    const data = confirm && JSON.stringify(confirm.reply_markup).match(/hbw:send:[0-9a-f]+/)[0];
    if (!data) { problems.push('red packet confirmation not shown for ' + type); return null; }
    await tap(xm, data, { message_id: 1, chat: pchat }); await wait(1500);
    return sql("select concat(code,' ',type,' ',total,' ',count,' ',chat_id,' ',message_id) from red_packets order by id desc limit 1").split(' ');
  };
  await say(xm, '/hb@e2e_bot_111', group); await wait(1500);
  if (!(await sentNow()).some(m => m.chat_id === -100555 && JSON.stringify(m.reply_markup || {}).includes('start=hb_-100555'))) problems.push('group /hb did not offer the private-chat link');
  const lucky = await sendPacket('lucky', ['3', '3', '新年快乐']);
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
  }
  const pw = await sendPacket('password', ['2', '2', '芝麻开门', 'default-greeting']);
  if (pw) {
    await say(xh, '芝麻开门', group); await wait(1500);
    if (!(await sentNow()).some(m => m.chat_id === -100555 && /小红 抢到/.test(m.text || ''))) problems.push('password claim in group failed');
  }
  const xmBal = sql(`select available from wallet_accounts where user_id=${uid} and currency='USDT'`);
  if (xmBal !== '95.000000') problems.push('sender not charged 5 USDT for two packets: ' + xmBal);
  await nav(admin, 'redpackets'); await shot(admin, '55-admin-redpackets');

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

  await browser.close();
  console.log('\nPROBLEMS (' + problems.length + '):\n' + problems.map(p => ' - ' + p).join('\n'));
  console.log('Screenshots: ' + SHOTS);
  process.exit(problems.length ? 1 : 0);
})().catch(e => { console.error('FATAL', e); console.log('\nPROBLEMS so far:\n' + problems.join('\n')); process.exit(1); });
