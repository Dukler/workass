import { execFileSync } from 'node:child_process';
// The person's side: no window at any point; the ask opens the page in the chat's panel.
export default async function panel({ tool, toolErr, tabs, probe, rEval, pageEval, sleep, note, step, S, OUT, popupTab, openerTab, frontApp, onScreen }) {
  step('visible:true without a reason is refused and shows nothing');
  note('ask_without_reason', await toolErr('workass_browser_open', { visible: true }));
  note('panel_after_refused_ask', await rEval(`!!document.querySelector('.live-browser')`));

  step('the explicit ask: this popup needs the person');
  const front = frontApp();
  await tool('workass_browser_open', { tab_id: popupTab.id, visible: true, reason: 'Aprobá el inicio de sesión en esta página' });
  await sleep(1500);
  const state = () => rEval(`(()=>{const sel=document.querySelector('.brw2pages');return {panel:!!document.querySelector('.live-browser'),float:!!document.querySelector('.fbrw'),picker:sel?{value:sel.value,options:[...sel.options].map(o=>o.textContent)}:null,ask:(document.querySelector('.brw2ask')||{}).innerText||null}})()`);
  note('after_ask', { ...(await state()), front: frontApp() === front, windows: (await probe('/windows')).windows.filter((w) => w.visible).map((w) => w.main ? 'main' : w.title), onScreen: onScreen().length });
  note('popup_when_shown', { tabVisible: (await tabs()).find((t) => t.id === popupTab.id)?.visible, url: await pageEval(popupTab.id, 'location.pathname') });
  await probe(`/shot?file=${OUT}/03-panel-with-popup.png`);
  // capturePage leaves native views out: what the window server has, page included.
  try { execFileSync('/usr/sbin/screencapture', ['-x', '-o', `-l${onScreen()[0].id}`, `${OUT}/03b-window.png`]); note('window_capture', true); } catch (error) { note('window_capture', String(error.message).slice(0, 120)); }

  step('the agent still works the page the person is looking at');
  await tool('workass_browser_screenshot', { tab_id: popupTab.id });
  await tool('workass_browser_click', { tab_id: popupTab.id, selector: '#approve' });
  note('click_on_shown_popup', await pageEval(popupTab.id, `document.getElementById('r').textContent`));

  step('the person picks the chat\'s own page in the panel');
  await rEval(`(()=>{const sel=document.querySelector('.brw2pages');const own=[...sel.options].find(o=>!o.value.includes('#tab'));const set=Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype,'value').set;set.call(sel,own.value);sel.dispatchEvent(new Event('change',{bubbles:true}));return own.value})()`);
  await sleep(1500);
  const list = await tabs();
  note('after_pick_own', { ...(await state()), visibleTabs: list.filter((t) => t.visible).map((t) => t.url), windows: (await probe('/windows')).windows.filter((w) => w.visible).length });

  step('the person closes the panel; everything is hidden and keeps working');
  await rEval(`document.querySelector('.brw2close').click()`);
  await sleep(1200);
  note('after_close_panel', { panel: await rEval(`!!document.querySelector('.live-browser')`), visibleTabs: (await tabs()).filter((t) => t.visible).length });
  const shot = await tool('workass_browser_screenshot', { tab_id: popupTab.id });
  note('screenshot_after_close', JSON.stringify(shot).slice(0, 200));
}
