const {chromium}=require('playwright');
const {finalizeEvent,generateSecretKey}=require('nostr-tools');
const fs=require('node:fs'),assert=require('node:assert/strict');
const meta=JSON.parse(fs.readFileSync('/evidence/meta.json'));
const root=new Uint8Array(fs.readFileSync('/signer/nostr.key'));
const negativeKeys=JSON.parse(fs.readFileSync('/signer/negative-keys.json'));const negativeResults={};
(async()=>{
 const browser=await chromium.launch({executablePath:'/usr/bin/chromium',headless:true,args:['--no-sandbox']});
 const contexts=[await browser.newContext(),await browser.newContext()];
 const [white,black]=await Promise.all(contexts.map(c=>c.newPage()));
 const errors=[];for(const p of [white,black])p.on('pageerror',e=>errors.push(String(e)));
 let signer=root;await white.exposeFunction('fixtureNostrSign',event=>finalizeEvent(event,signer));
 await white.addInitScript(()=>{window.nostr={signEvent:event=>window.fixtureNostrSign(event)}});
 const url=`http://127.0.0.1:${meta.port}/game?game=${encodeURIComponent(meta.game)}`;
 async function key(page){await page.locator('#create-tab-key').click();await page.waitForFunction(()=>/^[a-f0-9]{64}$/.test(document.querySelector('#identity-fingerprint').textContent));return page.locator('#identity-fingerprint').innerText()}
 async function anchor(page){await page.locator('#identity-scope').selectOption('chess');const response=page.waitForResponse(r=>r.url().endsWith('/v1/identity/endorsement/submit'));await page.locator('#anchor-nostr').click();const result=await (await response).json();await page.waitForFunction(()=>document.querySelector('#identity-message').textContent.includes('Nostr anchored'));await page.waitForFunction(head=>document.body.dataset.head===head,result.head);}
 async function prepare(page,uci){await page.locator(`[data-square="${uci.slice(0,2)}"]`).click();await page.locator(`[data-square="${uci.slice(2,4)}"].legal`).waitFor();await page.locator(`[data-square="${uci.slice(2,4)}"]`).click();await page.locator('#action-confirmation').waitFor({state:'visible'});}
 async function submit(page){const response=page.waitForResponse(r=>r.url().endsWith('/v1/actions/submit'));await page.locator('#sign-game-action').click();const r=await response;assert.equal(r.status(),200);return r.json()}
 async function move(page,uci){await prepare(page,uci);const r=await submit(page);assert.equal(r.effective,true,JSON.stringify(r));await page.waitForFunction(x=>document.querySelector('#last-move').textContent.includes(x),uci);return r;}
 try{
  await Promise.all([white.goto(url),black.goto(url)]);
  const firstKey=await key(white);await anchor(white);assert.notEqual(firstKey,meta.old_white_key);
  await black.locator('#join-game').click();await black.locator('#action-confirmation').waitFor({state:'visible'});const joined=await submit(black);assert.equal(joined.effective,true);
  await white.waitForFunction(()=>document.querySelector('.game-status').textContent.includes('white to move'));
  await move(white,'f2f3');await black.waitForFunction(()=>document.querySelector('#last-move').textContent.includes('f2f3'));
  await prepare(black,'e7e5');let original,accepted;
  await black.route('**/v1/actions/submit',async route=>{original=route.request().postData();const r=await route.fetch();accepted=await r.json();await route.abort('failed');await black.unroute('**/v1/actions/submit')});
  await black.locator('#sign-game-action').click();await black.getByRole('button',{name:'Retry the same signed action',exact:true}).waitFor();
  const retriedPromise=black.waitForResponse(r=>r.url().endsWith('/v1/actions/submit'));await black.getByRole('button',{name:'Retry the same signed action',exact:true}).click();const retryResponse=await retriedPromise,retried=await retryResponse.json();assert.equal(retryResponse.request().postData(),original);assert.equal(retried.record,accepted.record);assert.equal(retried.depth,accepted.depth);
  await white.waitForFunction(()=>document.querySelector('#last-move').textContent.includes('e7e5'));
  await white.reload();const unanchored=await key(white);assert.notEqual(unanchored,firstKey);await prepare(white,'g2g4');const noAnchor=await submit(white);assert.equal(noAnchor.effective,false);assert.match(noAnchor.reason,/seat|actor|identity|player/i);
  signer=generateSecretKey();await anchor(white);await prepare(white,'g2g4');const wrong=await submit(white);assert.equal(wrong.effective,false);
  for (const kind of ['expired','revoked']) {
   const context=await browser.newContext();const p=await context.newPage();
   await p.addInitScript(fixture=>{const original=crypto.subtle.generateKey.bind(crypto.subtle);crypto.subtle.generateKey=async (...args)=>{if(args[0].name!=='Ed25519')return original(...args);const bytes=s=>Uint8Array.from(atob(s),c=>c.charCodeAt(0));return {privateKey:await crypto.subtle.importKey('pkcs8',bytes(fixture.pkcs8),'Ed25519',false,['sign']),publicKey:await crypto.subtle.importKey('raw',bytes(fixture.public),'Ed25519',true,['verify'])}}},negativeKeys[kind]);
   await p.goto(url);assert.equal(await key(p),negativeKeys[kind].fingerprint);await prepare(p,'g2g4');const result=await submit(p);assert.equal(result.effective,false);negativeResults[kind]=result;await context.close();
  }
  await white.reload();const recoveredKey=await key(white);signer=root;await anchor(white);await move(white,'g2g4');await black.waitForFunction(()=>document.querySelector('#last-move').textContent.includes('g2g4'));
  const last=await move(black,'d8h4');await white.waitForFunction(()=>document.querySelector('.outcome').textContent.includes('Checkmate'));
  await white.screenshot({path:'/evidence/white-checkmate.png',fullPage:true});await black.screenshot({path:'/evidence/black-checkmate.png',fullPage:true});assert.deepEqual(errors,[]);
  const result={...meta,twoIsolatedBrowserContexts:true,platform:process.platform,chromium:await browser.version(),containerHostNetwork:true,firstKey,recoveredKey,oldKeyRecovery:true,tabReloadRecovery:true,unanchoredRefused:noAnchor,wrongIdentityRefused:wrong,preprovisionedNegativeKeys:negativeResults,exactLostResponseRetry:{record:retried.record,depth:retried.depth},final:last,outcome:await white.locator('.outcome').innerText(),pageErrors:errors};fs.writeFileSync('/evidence/browser-summary.json',JSON.stringify(result,null,2));console.log(JSON.stringify(result));
 }catch(e){await white.screenshot({path:'/evidence/white-failure.png',fullPage:true});await black.screenshot({path:'/evidence/black-failure.png',fullPage:true});console.error('WHITE',await white.locator('body').innerText());console.error('BLACK',await black.locator('body').innerText());throw e}
 finally{await browser.close()}
})().catch(e=>{console.error(e);process.exitCode=1});
