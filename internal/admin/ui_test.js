// Pure JavaScript behavior tests with a small DOM fixture; no browser or network access.
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const html=fs.readFileSync(__dirname+'/index.html','utf8');
function harness(){
 const elements=new Map(),requests=[];
 class Element{
  constructor(id,tag='div'){this.id=id;this.tag=tag;this.value='';this.hidden=false;this.checked=false;this.disabled=false;this.style={};this.content={textContent:''};this.attrs={};this.listeners={};this.classList={add(){},remove(){},toggle(){}}}
  set innerHTML(text){this.html=text;if(this.tag==='template'){this.content.textContent=text.replace(/<[^>]*>/g,'');return}parse(text)}
  get innerHTML(){return this.html||''}
  focus(){document.activeElement=this} setAttribute(k,v){this.attrs[k]=v} getBoundingClientRect(){return {left:20,top:100,bottom:145,width:240}} dispatchEvent(e){this['on'+e.type]?.(e);(this.listeners[e.type]||[]).forEach(fn=>fn(e));return true} addEventListener(name,fn){(this.listeners[name]??=[]).push(fn)} querySelector(){return null} querySelectorAll(){return []} replaceChildren(){} setSelectionRange(){} getClientRects(){return [1]}
 }
 function parse(source){
  for(const m of source.matchAll(/<(\w+)\b([^>]*\bid="([^"]+)"[^>]*)>/g)){
   const [,tag,attrs,id]=m,e=new Element(id,tag);e.value=(attrs.match(/\bvalue="([^"]*)"/)||[])[1]||'';e.checked=/\bchecked\b/.test(attrs);e.hidden=/\bhidden\b/.test(attrs);elements.set(id,e);
   if(tag==='select'){const end=source.indexOf('</select>',m.index);const opts=[...source.slice(m.index,end).matchAll(/<option value="([^"]*)"([^>]*)>/g)];e.value=(opts.find(x=>/selected/.test(x[2]))||opts[0]||[])[1]||''}
  }
 }
 parse(html.split('<script>')[0]);
 const fixtures={bots:[],routes:[],dashboard:{},pushes:[]};
 const document={getElementById:id=>elements.get(id),querySelector:()=>({content:''}),body:{style:{}},activeElement:null,addEventListener(){},createElement:tag=>new Element('',tag)};
 const context=vm.createContext({document,window:{innerHeight:900,addEventListener(){}},Event,location:{replace(){}},console,URLSearchParams,setTimeout(){return 1},clearTimeout(){},Date,Intl,FormData:class{},fetch:async(url,options={})=>{requests.push({url,options});return {ok:true,json:async()=>options.method?{id:1,ok:true}:(fixtures[url.replace('/admin/api/','')]||[])}}});
 const script=html.split('<script>')[1].split('</script>')[0];
 vm.runInContext(script.slice(0,script.lastIndexOf("$('brandIcon').innerHTML")),context);
 return {run:code=>vm.runInContext(code,context),elements,requests};
}
test('task actions distinguish pause, resume and permanent deletion',()=>{
 const h=harness();
 for(const state of ['scheduled','queued','processing']){const actions=h.run(`rowActions({id:1,status:'${state}'})`);assert.match(actions,/data-action="pause"/);assert.doesNotMatch(actions,/data-action="delete"/)}
 const paused=h.run("rowActions({id:1,status:'paused'})");assert.match(paused,/data-action="resume"/);assert.doesNotMatch(paused,/data-action="delete"/);
 const done=h.run("rowActions({id:1,status:'completed'})");assert.match(done,/data-action="delete"/);assert.doesNotMatch(done,/data-action="pause"/);
});
test('composer defaults to personal greeting and automatic business routing',async()=>{
 const h=harness();h.run('compose()');
 assert.equal(h.elements.get('pPersonalize').checked,true);
 h.elements.get('pTitle').value='招聘通知';h.elements.get('pContent').value='正文';h.elements.get('pCategory').value='招聘';
 await h.run('submitHandler()');
 const request=h.requests.find(x=>x.options.method==='POST');const payload=JSON.parse(request.options.body);
 assert.equal(payload.personalize,true);assert.equal(payload.category,'招聘');assert.equal(payload.bot_id,undefined);assert.equal(payload.content,'正文');
});
test('explicit bot and scheduled date are sent in creation payload',async()=>{
 const h=harness();h.run('compose()');
 for(const [id,v] of Object.entries({pTitle:'通知',pContent:'正文',pBot:'7',pWhen:'schedule',pSchedule:'2100-01-01T10:00'}))h.elements.get(id).value=v;
 await h.run('submitHandler()');const payload=JSON.parse(h.requests.find(x=>x.options.method==='POST').options.body);
 assert.equal(payload.bot_id,7);assert.match(payload.scheduled_at,/^2100-/);
});
test('scheduled messages reject missing or expired send times',async()=>{
 const h=harness();h.run('compose()');h.elements.get('pTitle').value='test';h.elements.get('pContent').value='test';h.elements.get('pWhen').value='schedule';
 await assert.rejects(h.run('submitHandler()'),/发送时间/);
 h.elements.get('pSchedule').value='2000-01-01T01:00';await assert.rejects(h.run('submitHandler()'),/未来/);
 assert.equal(h.requests.filter(x=>x.options.method==='POST').length,0);
});
test('task titles and bot names cannot inject markup into the table',()=>{
 const h=harness();const output=h.run(`pushRow({id:1,title:'<img src=x onerror=alert(1)>',status:'completed',sent_count:0,total_count:0,bot_id:0})`);
 assert.match(output,/&lt;img/);assert.doesNotMatch(output,/<img src=x/);
});

test('recurring composer submits interval and preserves first occurrence',async()=>{
 const h=harness();h.run('compose()');
 for(const [id,v] of Object.entries({pTitle:'每日通知',pContent:'正文',pWhen:'repeat',pSchedule:'2100-01-01T10:00',pInterval:'2',pUnit:'86400'}))h.elements.get(id).value=v;
 h.run("$('pWhen').onchange()");assert.equal(h.elements.get('repeatBox').hidden,false);assert.equal(h.elements.get('scheduleBox').hidden,false);
 await h.run('submitHandler()');const request=h.requests.find(x=>x.options.method==='POST');const payload=JSON.parse(request.options.body);
 assert.equal(payload.repeat_seconds,172800);assert.match(payload.scheduled_at,/^2100-/);assert.equal(request.options.headers['X-Requested-With'],'admin-console');
});
test('recurring composer rejects invalid intervals without creating a task',async()=>{
 const h=harness();h.run('compose()');for(const [id,v] of Object.entries({pTitle:'test',pContent:'text',pWhen:'repeat',pSchedule:'2100-01-01T10:00',pUnit:'86400'}))h.elements.get(id).value=v;
 for(const value of ['0','-1','1.5','367','abc']){h.elements.get('pInterval').value=value;await assert.rejects(h.run('submitHandler()'),/周期/)}
 assert.equal(h.requests.length,0);
});
test('custom dropdown selection updates native value, label and dependent form fields',()=>{
 const h=harness();h.run('compose()');
 h.run("openSelect('pWhen')");assert.equal(h.elements.get('pWhenOptions').hidden,false);assert.equal(h.elements.get('pWhenTrigger').attrs['aria-expanded'],'true');
 h.run("chooseOption({dataset:{owner:'pWhen',value:'repeat'},childNodes:[{textContent:'周期发送'}]})");
 assert.equal(h.elements.get('pWhen').value,'repeat');assert.equal(h.elements.get('pWhenText').textContent,'周期发送');assert.equal(h.elements.get('repeatBox').hidden,false);assert.equal(h.elements.get('pWhenOptions').hidden,true);assert.equal(h.elements.get('pWhenTrigger').attrs['aria-expanded'],'false');
 const markup=h.run("selectField('test','选项',[['a','<script>']], 'a')");assert.match(markup,/role="listbox"/);assert.match(markup,/role="option"/);assert.match(markup,/&lt;script&gt;/);assert.doesNotMatch(markup,/<script>/);
});
test('logout requests server revocation with CSRF header',async()=>{
 const h=harness();await h.run("$('logout').onclick()");const request=h.requests.find(x=>x.url==='/admin/logout');assert.equal(request.options.method,'POST');assert.equal(request.options.headers['X-Requested-With'],'admin-console');
});

test('menu editor submits reply placement, row and text response',async()=>{
 const h=harness();h.run("current='menus';edit()");
 for(const [id,value] of Object.entries({placement:'reply',code:'food',title:'🍴 美食',actionType:'text',description:'餐厅列表',row:'1',sort:'2',parent:'0',role:'user'}))h.elements.get(id).value=value;
 await h.run('submitHandler()');const request=h.requests.find(r=>r.url==='/admin/api/menus'&&r.options.method==='POST');const payload=JSON.parse(request.options.body);
 assert.equal(payload.placement,'reply');assert.equal(payload.row_no,1);assert.equal(payload.sort,2);assert.equal(payload.action_type,'text');assert.equal(payload.description,'餐厅列表');assert.equal(payload.title,'🍴 美食');
});

test('parent selector excludes self, descendants and non-submenu actions',()=>{
 const h=harness();h.run(`rows=[{id:1,title:'美食',code:'food',action_type:'submenu',parent_id:0},{id:2,title:'中餐',code:'chinese',action_type:'submenu',parent_id:1},{id:3,title:'面食',code:'noodles',action_type:'submenu',parent_id:2},{id:4,title:'购物',code:'shop',action_type:'submenu',parent_id:0},{id:5,title:'链接',code:'url',action_type:'url',parent_id:0}]`);
 const ids=Array.from(h.run('parentMenuOptions({id:1}).map(([id])=>id)'));assert.deepEqual(ids,[0,4]);
 assert.equal(h.run("current='menus';cell({parent_id:1},'parent_id')"),'美食');
});
test('nested bottom menu preserves both parent association and placement',async()=>{
 const h=harness();h.run("current='menus';rows=[{id:12,title:'美食',code:'food',action_type:'submenu',parent_id:0}];edit({id:13,parent_id:12,title:'中餐',code:'chinese',action_type:'text',placement:'inline'})");
 assert.equal(h.elements.get('parent').value,'12');assert.ok(h.elements.has('parentSearch'));
 h.run("chooseOption({dataset:{owner:'placement',value:'reply'},childNodes:[{textContent:'底部快捷菜单'}]})");
 assert.equal(h.elements.get('parent').value,'12');
 h.run("chooseOption({dataset:{owner:'parent',value:'12'},childNodes:[{textContent:'美食'}]})");
 assert.equal(h.elements.get('placement').value,'reply');
 await h.run('submitHandler()');const payload=JSON.parse(h.requests.find(r=>r.options.method==='POST').options.body);assert.equal(payload.parent_id,12);assert.equal(payload.placement,'reply');
});
test('search filters parent menu names and reports no matches',()=>{
 const h=harness();h.run("current='menus';edit();globalThis.choices=[{textContent:'美食 · food'},{textContent:'购物 · shop'}];$('parentOptions').querySelectorAll=()=>choices;filterSelect('parent','FOOD')");
 assert.equal(h.run('choices[0].hidden'),false);assert.equal(h.run('choices[1].hidden'),true);
 h.run("filterSelect('parent','不存在')");assert.equal(h.elements.get('parentEmpty').hidden,false);
});

test('merchant editor associates by child menu name and saves business details',async()=>{
 const h=harness();h.run("current='merchants';menuChoices=[{id:1,title:'美食',code:'food',action_type:'submenu',parent_id:0},{id:2,title:'中餐',code:'chinese',action_type:'merchants',parent_id:1},{id:3,title:'链接',code:'url',action_type:'url',parent_id:1}];edit()");
 const ids=Array.from(h.run('merchantMenuOptions().map(([id])=>id)'));assert.deepEqual(ids,[0,2]);assert.match(h.run('merchantMenuOptions()[1][1]'),/美食 \/ 中餐/);
 for(const [id,value] of Object.entries({merchantName:'餐馆',merchantMenu:'2',merchantDescription:'好吃',merchantAddress:'曼谷',merchantPhone:'123',merchantHours:'10–22',merchantContact:'https://t.me/shop',merchantSort:'5'}))h.elements.get(id).value=value;
 h.elements.get('merchantEnabled').checked=false;h.elements.get('merchantPinned').checked=true;
 await h.run('submitHandler()');const request=h.requests.find(r=>r.url==='/admin/api/merchants'&&r.options.method==='POST');const payload=JSON.parse(request.options.body);assert.equal(payload.menu_id,2);assert.equal(payload.pinned,true);assert.equal(payload.enabled,false);assert.equal(payload.name,'餐馆');assert.equal(payload.contact_url,'https://t.me/shop');assert.equal(payload.sort,5);
});
test('merchant creation requires both name and category',async()=>{
 const h=harness();h.run("current='merchants';edit()");await assert.rejects(h.run('submitHandler()'),/关联子菜单/);h.elements.get('merchantName').value='店铺';await assert.rejects(h.run('submitHandler()'),/关联子菜单/);assert.equal(h.requests.length,0);
});

test('photo menu submits image and contact button settings',async()=>{
 const h=harness();h.run("current='menus';edit()");
 for(const [id,value] of Object.entries({code:'contact',title:'☎ 客服',actionType:'photo',placement:'reply',menuImageURL:'https://example.com/contact.png',menuButtonText:'联系客服',actionValue:'https://t.me/contact'}))h.elements.get(id).value=value;
 await h.run('submitHandler()');
 const payload=JSON.parse(h.requests.find(r=>r.options.method==='POST').options.body);
 assert.equal(payload.action_type,'photo');assert.equal(payload.image_url,'https://example.com/contact.png');assert.equal(payload.button_text,'联系客服');assert.equal(payload.action_value,'https://t.me/contact');
});
test('account management is only listed for super administrators',()=>{
 const h=harness();
 h.run("me={id:2,role:'admin'};nav()");
 assert.ok(!h.elements.get('navigation').innerHTML.includes('data-nav="accounts"'));
 h.run("me={id:1,role:'super'};nav()");
 assert.ok(h.elements.get('navigation').innerHTML.includes('data-nav="accounts"'));
 h.run("current='accounts'");
 assert.ok(!h.run("rowActions({id:1})").includes('data-action="delete"'),'own account must not be deletable');
 assert.ok(h.run("rowActions({id:3})").includes('data-action="delete"'));
});
test('navigation lists only pages granted by the account role',()=>{
 const h=harness();
 h.run("me={id:2,role:'admin',permissions:['withdrawals','deposits']};nav()");
 const html=h.elements.get('navigation').innerHTML;
 assert.ok(html.includes('data-nav="withdrawals"')&&html.includes('data-nav="deposits"'));
 for(const page of ['pushes','configs','accounts','roles'])assert.ok(!html.includes(`data-nav="${page}"`),page);
 h.run("me={id:1,role:'super',permissions:[]};nav()");
 assert.ok(h.elements.get('navigation').innerHTML.includes('data-nav="roles"'));
});
test('shop categories follow the shop products permission and are not a grantable key',()=>{
 const h=harness();
 h.run("me={id:2,role:'admin',permissions:['shop_products']};nav()");
 const html=h.elements.get('navigation').innerHTML;
 assert.ok(html.includes('data-nav="shop_products"')&&html.includes('data-nav="shop_categories"'));
 assert.ok(!html.includes('data-nav="shop_orders"'));
 assert.equal(h.run("apiPath('shop_products')"),'shop-products');
});
test('merchant accounts see their shop pages but not categories or the platform home page',()=>{
 const h=harness();
 h.run("me={id:3,role:'merchant',merchant_name:'甲',permissions:['shop_products','shop_orders','shop_aftersales']};nav()");
 const html=h.elements.get('navigation').innerHTML;
 for(const page of ['shop_products','shop_orders','shop_aftersales'])assert.ok(html.includes(`data-nav="${page}"`),page);
 assert.ok(!html.includes('data-nav="shop_categories"'));
 assert.ok(!html.includes('data-nav="home"'));
});
test('home page shows to-dos, platform figures and per-currency money flows',()=>{
 const h=harness();
 h.run(`me={id:1,role:'super'};current='home';overview={range:'7d',since:'2026-10-17',todo:{pending_withdrawals:2,pending_deposits:1},platform:{users:30,new_users:4,merchants:3,enabled_merchants:2},shop:{orders:5,by_status:{paid:2,completed:3},paid:'100.5',refunded:'10',net:'90.5'},operations:{users:{referred:2,commission:'1.5'},shop:{valid_orders:4,avg_order:'22.625',commission:'3.2'},redpackets:{claims:9},service:{push_sent:9,push_failed:1}},trend:[{day:'2026-10-23',new_users:3,orders:2,sales:'40',deposits:'500',withdraws:'0',packets:1}],currencies:[{code:'USDT',name:'USDT-TRC20',deposit_count:3,deposit_amount:'500',withdraw_count:1,withdraw_amount:'620',withdraw_fee:'1.25',withdraw_actual:'618.75',net_flow:'-120',packet_count:2,packet_amount:'20',balance_available:'1234.5'},{code:'CNY',name:'人民币<b>',credit_amount:'88'}]};nav();render()`);
 const html=h.elements.get('content').innerHTML;
 assert.ok(h.elements.get('navigation').innerHTML.includes('data-nav="home"'));
 for(const text of ['提现手续费','1.25','618.75','运营统计','有效订单','22.625','平台佣金收入','送达率 90.0%','每日趋势','2026-10-23','待审提现','data-go="withdrawals"','近 7 天','2026-10-17 至今','90.5','待处理 2 · 已完成 3','充提差额','class="negative">-120','1,234.5','红包个数','人民币&lt;b&gt;'])assert.ok(html.includes(text),text);
});
test('merchant accounts get reports, settlements and reviews but not marketing',()=>{
 const h=harness();
 h.run("me={id:3,role:'merchant',permissions:['shop_products','shop_orders','shop_aftersales','shop_reviews','shop_reports','shop_settlements']};nav()");
 const html=h.elements.get('navigation').innerHTML;
 for(const page of ['shop_reports','shop_settlements','shop_reviews'])assert.ok(html.includes(`data-nav="${page}"`),page);
 assert.ok(!html.includes('data-nav="shop_marketing"'));
 h.run("reportRange={from:'2026-10-01',to:'2026-10-07'}");
 assert.equal(h.run("reportQuery()"),'?from=2026-10-01&to=2026-10-07');
});
test('navigation hides section titles that have no visible pages',()=>{
 const h=harness();
 h.run("me={id:3,role:'merchant_staff',permissions:['shop_orders']};nav()");
 const html=h.elements.get('navigation').innerHTML;
 assert.ok(html.includes('商城管理'));
 for(const title of ['运营工作台','业务管理','系统设置'])assert.ok(!html.includes(title),title);
});
test('audit log rows show logins and failed statuses distinctly',()=>{
 const h=harness();
 h.run("current='audit_logs'");
 assert.ok(h.run("cell({method:'LOGIN'},'method')").includes('登录'));
 assert.ok(h.run("cell({status:403},'status')").includes('red'));
 assert.ok(h.run("cell({status:200},'status')").includes('green'));
});
test('only merchant owners see staff management; staff and super admins do not',()=>{
 const h=harness();
 h.run("me={id:3,role:'merchant',permissions:['shop_orders']};nav()");
 assert.ok(h.elements.get('navigation').innerHTML.includes('data-nav="merchant_staff"'));
 h.run("me={id:4,role:'merchant_staff',permissions:['shop_orders']};nav()");
 assert.ok(!h.elements.get('navigation').innerHTML.includes('data-nav="merchant_staff"'));
 h.run("me={id:1,role:'super',permissions:[]};nav()");
 assert.ok(!h.elements.get('navigation').innerHTML.includes('data-nav="merchant_staff"'));
});
test('every console page loads from an API route that exists',()=>{
 const fs=require('fs'),path=require('path');
 const routes=new Set();
 for(const f of fs.readdirSync(__dirname).filter(f=>f.endsWith('.go')&&!f.endsWith('_test.go')))
{const src=fs.readFileSync(path.join(__dirname,f),'utf8');
  for(const m of src.matchAll(/api\.GET\("\/([^"]+)"/g))routes.add(m[1]);
  // Groups such as accounts := api.Group("/accounts") with accounts.GET("", ...)
  for(const m of src.matchAll(/(\w+) := api\.Group\("\/([^"]+)"/g))if(src.includes(`${m[1]}.GET("",`))routes.add(m[2]);}
 const h=harness();
 const missing=h.run('Object.keys(pages)').filter(k=>!routes.has(h.run(`apiPath(${JSON.stringify(k)})`)));
 assert.equal(missing.join(','),'');
});
test('navigation groups collapse and expand, keeping the open page visible',()=>{
 const h=harness();
 h.run("me={id:1,role:'super'};current='home';nav()");
 let html=h.elements.get('navigation').innerHTML;
 for(const g of ['运营工作台','业务管理','商城管理','商城经营','新闻资讯','系统设置'])assert.ok(html.includes(`data-group="${g}"`),g);
 assert.ok(!html.includes('data-group="概览"'));
 h.run("toggleGroup('业务管理')");html=h.elements.get('navigation').innerHTML;
 assert.ok(!html.includes('data-nav="withdrawals"'));assert.ok(html.includes('aria-expanded="false"'));
 h.run("current='withdrawals';nav()");assert.ok(h.elements.get('navigation').innerHTML.includes('data-nav="withdrawals"'));
 h.run("current='home';toggleGroup('业务管理')");assert.ok(h.elements.get('navigation').innerHTML.includes('data-nav="withdrawals"'));
});
test('shop order, after-sale and review details are in Chinese with the buyer',()=>{
 const h=harness();
 const text=code=>h.run(code).map(([k,v])=>k+'：'+v).join('\n');
 h.run("current='shop_orders'");
 const order=text(`shopDetail({order_no:'S1',merchant_name:'甲',user_id:5,telegram_user_id:9001,username:'xiaoming',user_name:'小明',status:'paid',delivery_type:'express',items:[{product_name:'茶',sku_name:'大份',quantity:2,price:'5'}],goods_amount:'10',discount:'1',member_discount:'0',points_discount:'0',platform_subsidy:'1',pay_amount:'9',refunded_amount:'0',contact_name:'张三',address:'曼谷',created_at:'2026-10-23T10:00:00Z',commission_done:false})`);
 for(const t of ['订单编号','小明 · @xiaoming · TG 9001','茶 · 大份 × 2','优惠券：−1 USDT','平台补贴','实付金额','收货地址：曼谷'])assert.ok(order.includes(t),t);
 assert.ok(!/会员折扣|积分抵扣|快递单号|order_no|created_at/.test(order));
 h.run("current='shop_reviews'");
 const review=text(`shopDetail({order_no:'S1',product_name:'茶',sku_name:'A牌',buyer_name:'R**',username:'r',telegram_user_id:7,user_id:1,rating:5,content:'非常nice',hidden:false,reply:'',created_at:'2026-10-23T10:00:00Z'})`);
 for(const t of ['评价内容：非常nice','正常显示','5 分','@r'])assert.ok(review.includes(t),t);
 assert.ok(!/false|null|sku_name|order_item_id/.test(review));
 h.run("current='tickets'");
 const plain=text(`plainDetail({id:3,subject:'x',status:'open',created_at:'2026-10-23T10:00:00Z',replied_at:null,secret_field:1})`);
 assert.ok(plain.includes('工单：3')&&plain.includes('待回复')&&!plain.includes('secret_field'));
});
