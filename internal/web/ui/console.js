'use strict';

const get = selector => document.querySelector(selector);
const session = { token: '', who: null, section: '', generation: 0 };
const labels = { 'invalid-input':'请核对字段与格式', 'access-denied':'授权不足或已到期', missing:'记录当前不可用',
  conflict:'当前版本发生变化或请求有冲突', forgotten:'来源已被遗忘水位拦截', 'recovery-required':'服务需要先完成恢复检查',
  'model-unavailable':'模型暂时无法提供有效输出', internal:'服务处理失败' };
let noticeTimer;

function el(tag, content, css) {
  const node = document.createElement(tag);
  if (content !== undefined) node.textContent = content;
  if (css) node.className = css;
  return node;
}

async function api(path, body, method = body === undefined ? 'GET' : 'POST') {
  const headers = {};
  if (session.token) headers.Authorization = 'Bearer ' + session.token;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch('/api/' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const decoded = await response.json();
  if (!response.ok) {
    if (decoded.conflict_id) throw new Error('来源编号已被使用，冲突回执 #' + decoded.conflict_id);
    if (decoded.job_id) throw new Error('模型处理失败，任务编号 ' + decoded.job_id);
    throw new Error(labels[decoded.error] || '请求失败');
  }
  return decoded;
}

function notify(message, error = false) {
  const target = get('#notice');
  if (!target) return;
  target.textContent = message;
  target.toggleAttribute('data-error', error);
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => { target.textContent = ''; }, 6500);
}

async function perform(button, action) {
  button.disabled = true;
  try { await action(); }
  catch (error) { notify(error.message, true); }
  finally { button.disabled = false; }
}

function button(title, action, plain = false) {
  const node = el('button', title, plain ? 'plain' : undefined);
  node.type = 'button';
  node.addEventListener('click', () => perform(node, action));
  return node;
}

function panel(title) {
  const node = el('section', undefined, 'panel');
  node.append(el('h2', title));
  return node;
}

function form(fields, title, run) {
  const node = el('form');
  for (const item of fields) {
    const [name, text, type = 'text', preset = '', optional = false] = item;
    const label = el('label', text, type === 'checkbox' ? 'choice' : undefined);
    const input = el(type === 'area' ? 'textarea' : 'input');
    input.name = name;
    input.value = preset;
    if (type === 'checkbox') {
      input.type = type;
      input.checked = Boolean(preset);
      label.prepend(input);
    } else {
      if (type !== 'area') input.type = type;
      input.required = !optional;
      label.append(input);
    }
    node.append(label);
  }
  const submit = el('button', title);
  node.append(submit);
  node.addEventListener('submit', event => {
    event.preventDefault();
    perform(submit, () => run(Object.fromEntries(new FormData(node))));
  });
  return node;
}

function showJSON(target, value) { target.textContent = JSON.stringify(value, null, 2); }
function key() { return crypto.randomUUID().toLowerCase(); }
function iso(value) { return new Date(value).toISOString(); }
function date(value) {
  const when = new Date(value);
  return new Date(when.getTime() - when.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}
function receipt(title, fields, action) {
  const node = panel(title);
  const output = el('pre');
  output.hidden = true;
  node.append(form(fields, '提交', async input => {
    const result = await action(input);
    showJSON(output, result);
    output.hidden = false;
  }), output);
  return node;
}

const sections = {
  facts: ['当前记忆', async target => {
    const data = await api('export');
    if (!target.isConnected) return;
    const bar = panel('我的资料');
    bar.append(button('下载 JSON', () => {
      const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type:'application/json' }));
      const link = el('a');
      link.download = 'memory-export.json';
      link.href = url;
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    }));
    target.append(bar);
    const cards = el('div', undefined, 'grid');
    for (const fact of data.facts) {
      const card = panel(fact.slot + ' · R' + fact.version);
      card.append(el('p', fact.value), el('small', fact.purpose + ' / ' + fact.source.app + ' / ' + fact.source.external_id));
      if (fact.source.evidence) card.append(el('blockquote', fact.source.evidence));
      const actions = el('div', undefined, 'actions');
      actions.append(button('纠正', async () => {
        const value = window.prompt('新的记忆内容', fact.value);
        if (value?.trim() && value !== fact.value) {
          await api('edit', { slot:fact.slot, value:value.trim(), purpose:fact.purpose, key:key(), expected_version:fact.version });
          await switchTo('facts');
        }
      }), button('分享此版本', async () => {
        const share = await api('share', { slot:fact.slot, purpose:fact.purpose, minutes:20 });
        const url = location.origin + '/share#' + share.token;
        if (!card.isConnected) return;
        const row = el('div');
        const field = el('input');
        field.value = url;
        field.readOnly = true;
        field.setAttribute('aria-label', '20 分钟分享链接');
        row.append(field, button('撤回此链接', async () => {
          await api('share/revoke', { token:share.token, key:key() });
          row.remove();
          notify('分享已撤回');
        }, true));
        card.append(row);
      }, true));
      card.append(actions);
      cards.append(card);
    }
    if (!data.facts.length) cards.append(el('p','当前没有已生效的记忆。','hint'));
    target.append(cards);
  }],
  pending: ['候选审核', async target => {
    const rows = await api('pending');
    if (!target.isConnected) return;
    if (!rows.length) target.append(el('p','当前没有待审核候选。','hint'));
    const cards = el('div', undefined, 'grid');
    const selections = new Map();
    const entries = new Map();
    const summary = el('p');
    function reviewInput(item, accept) {
      return { candidate:item.id, expected_version:item.current_version, accept };
    }
    async function decideSelected(accept) {
      if (!selections.size || selections.size > 50) throw new Error('请选择 1 至 50 条候选');
      const results = await api('review/batch', [...selections.values()].map(item => reviewInput(item, accept)));
      let completed = 0;
      for (const result of results) {
        const entry = entries.get(result.candidate);
        if (result.error) {
          entry.message.textContent = labels[result.error] || '尚未处理';
        } else {
          entry.card.remove();
          selections.delete(result.candidate);
          completed++;
        }
      }
      summary.textContent = '已处理 ' + completed + ' 条，保留 ' + (results.length - completed) + ' 条待处理选项';
    }
    if (rows.length) {
      const batch = panel('批量裁决');
      batch.append(button('接受所选', () => decideSelected(true)), button('拒绝所选', () => decideSelected(false), true), summary);
      target.append(batch);
    }
    for (const item of rows) {
      const card = panel(item.slot + ' · ' + item.status);
      const confidenceLabels = { 'explicit-preference':'明确的长期偏好', 'evidence-matched':'证据与原文吻合，待本人确认' };
      card.append(el('p', item.value), el('blockquote', item.evidence),
        el('small', item.purpose + ' / ' + (confidenceLabels[item.confidence] || item.extraction)));
      card.append(el('small', item.provenance.app + ' / ' + item.provenance.session + ' / ' + item.provenance.external_id));
      if (item.current) {
        card.append(el('p', '当前记忆 R' + item.current_version + ' · ' + item.current.value),
          el('small', item.current.source.app + ' / ' + item.current.source.session + ' / ' + item.current.source.external_id));
        if (item.current.source.evidence) card.append(el('blockquote', item.current.source.evidence));
      }
      const actions = el('div', undefined, 'actions');
      async function decide(accept) {
        await api('review', reviewInput(item, accept));
        selections.delete(item.id);
        card.remove();
      }
      const checkbox = el('input');
      checkbox.type = 'checkbox';
      checkbox.addEventListener('change', () => {
        if (checkbox.checked) selections.set(item.id, item);
        else selections.delete(item.id);
      });
      const choice = el('label','选中此候选','choice');
      choice.prepend(checkbox);
      const message = el('p');
      entries.set(item.id, {card, message});
      actions.append(button('接受', () => decide(true)), button('拒绝', () => decide(false), true));
      card.append(choice,actions,message);
      cards.append(card);
    }
    target.append(cards);
  }],
  conflicts: ['来源冲突', async target => {
    const cards = el('div', undefined, 'grid');
    target.append(cards);
    async function load(after) {
      const page = await api('source-conflicts?after=' + encodeURIComponent(after));
      if (!target.isConnected) return;
      cards.replaceChildren();
      if (!page.items.length) cards.append(el('p', '当前页没有待处理的来源冲突。', 'hint'));
      for (const item of page.items) {
        const card = panel(item.external_id);
        card.append(el('p', '冲突回执 #' + item.id), el('small', item.app + ' / ' + item.purpose + ' / ' + item.session),
          el('p', new Date(item.recorded).toLocaleString()),
          button('确认保留原记录', async () => {
            await api('source-conflicts/dismiss', { id:item.id });
            await load(after);
          }));
        cards.append(card);
      }
      if (after !== '0') cards.append(button('返回第一页', () => load('0'), true));
      if (page.next_after) cards.append(button('下一页', () => load(page.next_after)));
    }
    await load('0');
  }],
  consent: ['用途授权', async target => {
    const data = await api('export');
    if (!target.isConnected) return;
    const grid = el('div', undefined, 'grid');
    grid.append(receipt('新增用途授权', [
      ['app','应用标识'],['purpose','用途','text','support'],
      ['expires','有效至（本地时间）','datetime-local',date(Date.now()+86400000)],['read','读','checkbox',true],
      ['write','写','checkbox',true],['auto','自动采信明确陈述','checkbox'],['model','允许发送文本至服务配置的模型','checkbox']
    ], async values => {
      await api('consent',{ app:values.app, purpose:values.purpose, expires:iso(values.expires), read:!!values.read,
        write:!!values.write, auto_accept:!!values.auto, allow_model:!!values.model });
      await switchTo('consent');
      return { applied:true };
    }));
    const active = panel('有效授权');
    for (const grant of data.consents) {
      const row = el('div', undefined, 'actions');
      row.append(el('span', grant.app + ' / ' + grant.purpose + ' · ' + [grant.read && '读', grant.write && '写'].filter(Boolean).join('、')),
        button('撤销', async () => {
          await api('consent/revoke', { app:grant.app, purpose:grant.purpose, key:key() });
          await switchTo('consent');
        }, true));
      active.append(row);
    }
    if (!data.consents.length) active.append(el('p','暂无有效授权。','hint'));
    grid.append(active);
    target.append(grid);
  }],
  recall: ['召回与解释', target => {
    const owner = session.who.role === 'owner';
    target.append(receipt('查询记忆', [['subject','用户标识','text',owner ? session.who.actor : ''],
      ['purpose','用途','text','support'],['slot','事实键','text','',true],['query','检索词','text','',true],
      ['time','历史时点（本地时间）','datetime-local','',true]], values => {
      const body = { subject:values.subject, purpose:values.purpose, slot:values.slot, query:values.query };
      if (values.time) body.as_of = iso(values.time);
      return api('recall',body);
    }));
  }],
  source: ['应用写入', target => {
    const grid = el('div', undefined, 'grid');
    const source = receipt('登记来源', [['subject','用户标识'],['purpose','用途','text','support'],
      ['session','会话标识'],['external_id','来源编号'],['time','事件时间（本地时间）','datetime-local',date(Date.now()-60000)],
      ['kind','来源分类（user / quotation / transient）','text','user'],['statement','来源文本','area']],
    values => api('source',{ subject:values.subject, purpose:values.purpose, session:values.session, external_id:values.external_id,
      occurred:iso(values.time), kind:values.kind, statement:values.statement.trim() }));
    const status = receipt('查询提取进度', [['id','任务编号']],
      values => api('extract/jobs?id=' + encodeURIComponent(values.id)));
    const extract = receipt('提交提取任务', [['subject','用户标识'],['purpose','用途','text','support'],['source','来源摘要']],
      async values => {
        const result = await api('extract/jobs', values);
        if (result.job_id) status.querySelector('input[name="id"]').value = result.job_id;
        return result;
      });
    grid.append(source,extract,status);
    target.append(grid);
  }],
  forget: ['遗忘与撤回', target => {
    const status = receipt('查询遗忘进度', [['id','操作编号']],
      values => api('forget?id=' + encodeURIComponent(values.id)));
    const node = receipt('遗忘记忆', [['slot','事实键（留空时清除本人全部记忆）','text','',true],
      ['confirm','输入 forget 以确认']], async values => {
      if (values.confirm !== 'forget') throw new Error('请输入 forget');
      const result = await api('forget',{ slot:values.slot, all:values.slot === '', key:key() });
      status.querySelector('input[name="id"]').value = result.id;
      return result;
    });
    target.append(node,status,receipt('撤回分享链接', [['url','分享链接']], values => {
      const url = new URL(values.url);
      if (url.origin !== location.origin || url.pathname !== '/share') throw new Error('请使用本站分享链接');
      return api('share/revoke',{ token:url.hash.slice(1), key:key() });
    }));
  }],
  extraction: ['提取进度', target => {
    target.append(receipt('按编号查询提取进度', [['id','任务编号']],
      values => api('extract/jobs?id=' + encodeURIComponent(values.id))));
  }],
  audit: ['操作审计', target => {
    target.append(receipt('读取摘要', [['after','起始游标','number','0']], values => api('audit?after=' + encodeURIComponent(values.after))));
    if (session.who.role !== 'admin') return;
    target.append(receipt('保留策略', [['raw','原文保留天数','number','14'],['facts','事实保留天数','number','180']],
      values => api('policy',{ source_days:Number(values.raw), fact_days:Number(values.facts) },'PUT')));
    const sweep = panel('过期回收');
    const details = el('pre');
    details.hidden = true;
    sweep.append(button('清理已过期内容', async () => {
      showJSON(details, await api('sweep',{}));
      details.hidden = false;
    }), details);
    target.append(sweep);
  }]
};

async function switchTo(section) {
  session.section = section;
  const generation = ++session.generation;
  const target = el('div');
  get('#content').replaceChildren(target);
  get('#title').textContent = sections[section][0];
  for (const item of get('#nav').children) item.setAttribute('aria-current', String(item.dataset.section === section));
  try { await sections[section][1](target); }
  catch (error) {
    if (generation === session.generation) target.replaceChildren(el('p',error.message,'panel'));
  }
}

async function startShare() {
  const token = location.hash.slice(1);
  history.replaceState(null,'','/share');
  const target = get('#shared');
  try {
    const fact = await api('shared',{ token });
    target.replaceChildren(el('h1',fact.slot),el('p',fact.value),el('small','版本 ' + fact.version));
  } catch (error) { target.replaceChildren(el('h1','共享资料'),el('p',error.message)); }
}

if (get('#shared')) {
  startShare();
} else {
  get('#login form').addEventListener('submit', event => {
    event.preventDefault();
    perform(event.submitter, async () => {
      const token = new FormData(event.currentTarget).get('token');
      session.token = token.trim();
      try { session.who = await api('session'); }
      catch (error) { session.token = ''; throw error; }
      const allowed = { owner:['facts','pending','conflicts','consent','recall','forget','extraction','audit'], app:['source','recall'],
        admin:['audit'], audit:['audit'] }[session.who.role];
      for (const section of allowed) {
        const item = button(sections[section][0], () => switchTo(section), true);
        item.dataset.section = section;
        get('#nav').append(item);
      }
      get('#session-name').textContent = session.who.tenant + ' / ' + session.who.actor;
      get('#login input').value = '';
      get('#login').hidden = true;
      get('#workspace').hidden = false;
      get('#refresh').hidden = false;
      await switchTo(allowed[0]);
    });
  });
  get('#refresh').addEventListener('click', () => switchTo(session.section));
  get('#disconnect').addEventListener('click', () => location.replace('/'));
}
