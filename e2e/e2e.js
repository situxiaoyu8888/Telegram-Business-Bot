// End-to-end walk through the admin console, the H5 shop (in a simulated Telegram Mini App)
// and bot commands. Started by run.sh, which provides the app, database and fake Bot API.
const { chromium } = require('playwright');
const crypto = require('crypto');
const fs = require('fs');
const { execSync } = require('child_process');

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:18731', TG = process.env.E2E_TG || 'http://127.0.0.1:38081', TOKEN = '111:primary';
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
  page.on('response', r => { if (r.status() >= 400) problems.push(`[${name}] HTTP ${r.status()} ${r.url()}`); });
}
async function shot(page, name) { await page.waitForTimeout(400); await page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: false }); }
async function adminApi(page, path, method = 'GET', body) {
  return page.evaluate(async ([path, method, body]) => {
    const r = await fetch('/admin/api/' + path, { method, headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'admin-console' }, body: body ? JSON.stringify(body) : undefined });
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

  // ---------- Admin ----------
  const actx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const admin = await actx.newPage(); watch(admin, 'admin');
  step('admin login');
  await admin.goto(BASE + '/admin/login');
  await admin.fill('#username', 'admin'); await admin.fill('#password', 'e2e-password-123');
  await Promise.all([admin.waitForURL('**/admin/'), admin.click('#submit')]);
  await admin.waitForTimeout(800); await shot(admin, '01-admin-home');

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
  await shopPage.goto(BASE + '/shop/#/');
  await shopPage.waitForTimeout(1200); await shot(shopPage, '10-shop-home');
  await shopPage.goto(BASE + '/shop/#/me'); await shopPage.waitForTimeout(800);
  const uid = sql("select id from telegram_users where telegram_user_id=9001");
  sql(`update wallet_accounts set available=100 where user_id=${uid}`);
  await shopPage.reload(); await shopPage.waitForTimeout(800); await shot(shopPage, '11-shop-me');

  step('claim coupon');
  await shopPage.goto(BASE + '/shop/#/coupons/center'); await shopPage.waitForTimeout(800);
  await shopPage.click('[data-claim]'); await shopPage.waitForTimeout(600); await shot(shopPage, '12-coupon-center');

  step('product page → cart');
  await shopPage.goto(BASE + '/shop/#/'); await shopPage.waitForTimeout(800);
  await shopPage.locator('#grid .tile').filter({ hasText: '宫保鸡丁' }).first().click();
  await shopPage.waitForTimeout(800); await shot(shopPage, '13-product');
  await shopPage.click('[data-sku]:has-text("大份")'); await shopPage.click('#plus');
  await shot(shopPage, '14-product-promo-sku');
  await shopPage.click('#addCart'); await shopPage.waitForTimeout(500);
  await shopPage.click('#shareBtn'); await shopPage.waitForTimeout(500);
  const shared = await shopPage.evaluate(() => window.__opened || '');
  if (!shared.includes('start%3Dp' + meal.id + '_')) problems.push('share link wrong: ' + shared);

  step('cart → checkout (express, new address, coupon)');
  await shopPage.goto(BASE + '/shop/#/cart'); await shopPage.waitForTimeout(800); await shot(shopPage, '15-cart');
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
  await shopPage.goto(BASE + '/shop/#/product/' + card.id); await shopPage.waitForTimeout(800);
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
  await shopPage.goto(BASE + '/shop/#/order/' + orderId); await shopPage.waitForTimeout(900); await shot(shopPage, '21-order-shipped');
  await shopPage.click('#confirmOrder'); await shopPage.waitForTimeout(1000);
  await shopPage.goto(BASE + '/shop/#/review/' + orderId); await shopPage.waitForTimeout(800);
  await shopPage.click('[data-star$=":4"]'); await shopPage.fill('[data-text]', '味道正宗，送得很快！');
  await shot(shopPage, '22-review-form');
  await shopPage.click('#rvSubmit'); await shopPage.waitForTimeout(900);
  await shopPage.goto(BASE + '/shop/#/aftersale/' + orderId); await shopPage.waitForTimeout(800);
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
  await shopPage.goto(BASE + '/shop/#/product/' + meal.id); await shopPage.waitForTimeout(900);
  await shopPage.evaluate(() => window.scrollTo(0, document.body.scrollHeight)); await shot(shopPage, '30-product-reviews');
  await shopPage.goto(BASE + '/shop/#/orders'); await shopPage.waitForTimeout(800); await shot(shopPage, '31-orders');

  step('group buying: start, share, friend joins');
  await shopPage.goto(BASE + '/shop/#/product/' + meal.id); await shopPage.waitForTimeout(900);
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
  await fpage.goto(BASE + '/shop/#/me'); await fpage.waitForTimeout(800);
  const fid = sql('select id from telegram_users where telegram_user_id=9003');
  sql(`update wallet_accounts set available=50 where user_id=${fid}`);
  await fpage.goto(BASE + '/shop/#/group/' + groupId); await fpage.waitForTimeout(900); await shot(fpage, '35-friend-group');
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
  await shopPage.goto(BASE + '/shop/#/me'); await shopPage.waitForTimeout(800);

  step('buyer contacts merchant, admin replies');
  await shopPage.goto(BASE + '/shop/#/product/' + meal.id); await shopPage.waitForTimeout(900);
  await shopPage.click('text=联系商家'); await shopPage.waitForTimeout(800);
  await shopPage.fill('#chatText', '请问大份可以少放辣吗？'); await shopPage.click('#chatSend'); await shopPage.waitForTimeout(900);
  await shot(shopPage, '37-chat-buyer');
  await nav(admin, 'shop_messages'); await shot(admin, '38-admin-messages');
  const convId = sql('select id from shop_conversations order by id desc limit 1');
  await admin.click(`[data-action="chat"][data-id="${convId}"]`); await admin.waitForTimeout(600);
  await admin.fill('#chatReply', '可以，下单时备注“少辣”即可。'); await shot(admin, '39-admin-chat');
  await submitModal(admin);
  await shopPage.goto(BASE + '/shop/#/chat/' + meal.merchant_id); await shopPage.waitForTimeout(900); await shot(shopPage, '40-chat-replied');
  const replyNotice = (await (await fetch(TG + '/sent')).json()).find(m => m.chat_id === 9001 && /回复了你/.test(m.text || ''));
  if (!replyNotice || !JSON.stringify(replyNotice.reply_markup || {}).includes('#/chat/')) problems.push('buyer not notified of merchant reply');

  step('friend applies to become a merchant, admin approves');
  await fpage.goto(BASE + '/shop/#/apply'); await fpage.waitForTimeout(800);
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
    await mpage.goto(BASE + '/admin/login');
    await mpage.fill('#username', appRow.split(' ')[1]); await mpage.fill('#password', password);
    await Promise.all([mpage.waitForURL('**/admin/'), mpage.click('#submit')]);
    await mpage.waitForTimeout(1000); await shot(mpage, '44-merchant-console');
    const navHtml = await mpage.innerHTML('#navigation');
    if (!navHtml.includes('data-nav="shop_products"') || navHtml.includes('data-nav="configs"') || navHtml.includes('data-nav="withdrawals"')) problems.push('merchant console shows wrong pages');
    await mctx.close();
  }
  await fpage.goto(BASE + '/shop/#/apply'); await fpage.waitForTimeout(700); await shot(fpage, '45-apply-approved');

  step('batch import products via the console');
  await nav(admin, 'shop_products');
  const exported = await admin.evaluate(async () => (await fetch('/admin/api/shop-products/export')).text());
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
  await owner.goto(BASE + '/admin/login');
  await owner.fill('#username', 'meal-owner'); await owner.fill('#password', 'owner-pass-123');
  await Promise.all([owner.waitForURL('**/admin/'), owner.click('#submit')]);
  await owner.waitForTimeout(800); await nav(owner, 'shop_settlements'); await shot(owner, '48-owner-settlements');
  await owner.click('[data-payout]'); await owner.fill('#payoutAddr', 'TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7'); await submitModal(owner);
  await owner.waitForTimeout(600); await shot(owner, '49-owner-payout-requested');
  const payout = sql(`select concat(status,' ',payout_address,' ',net) from shop_settlements order by id desc limit 1`);
  console.log('  payout', payout);
  if (!payout.startsWith('pending TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7')) problems.push('payout request not recorded: ' + payout);
  await octx.close();

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

  await browser.close();
  console.log('\nPROBLEMS (' + problems.length + '):\n' + problems.map(p => ' - ' + p).join('\n'));
  console.log('Screenshots: ' + SHOTS);
  process.exit(problems.length ? 1 : 0);
})().catch(e => { console.error('FATAL', e); console.log('\nPROBLEMS so far:\n' + problems.join('\n')); process.exit(1); });
