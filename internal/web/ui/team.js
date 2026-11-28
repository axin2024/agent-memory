'use strict';

{
  const picker = value => encodeURIComponent(value);
  const path = space => 'team-spaces/' + picker(space.space_id);
  const purpose = space => '?purpose=' + picker(space.purpose);
  const keyHeader = value => ({ 'Idempotency-Key': value });
  const checkbox = (name, label, selected = false) => [name, label, 'checkbox', selected];
  const states = { awaiting_review: '待双人审核', published: '已发布', rejected: '已退回', disputed: '待处理冲突',
    withdrawn: '已撤回', superseded: '已替代' };

  function showReceipt(node, data) {
    showJSON(node, data);
    node.hidden = false;
  }

  function claimCard(claim) {
    const node = panel(claim.topic + ' · ' + (states[claim.review_state] ?? claim.review_state));
    node.append(el('p', '版本 ' + claim.claim_rev + ' · ' + claim.claim_id));
    if (claim.applicability) node.append(el('p', '适用范围 · ' + claim.applicability));
    node.append(el('p', '自 ' + new Date(claim.valid_from).toLocaleString() + ' 至 ' + new Date(claim.expires_at).toLocaleString()));
    if (claim.explanation) node.append(el('p', claim.explanation + ' · ' + claim.score));
    for (const [index, sentence] of (claim.sentences ?? []).entries()) {
      node.append(el('h3', sentence));
      for (const cite of (claim.citations ?? []).filter(item => item.sentence === index)) {
        node.append(el('blockquote', cite.excerpt));
        node.append(el('small', '来源 ' + cite.source + ' · 授权 v' + cite.grant_rev + ' · 字节 ' + cite.start + '–' + cite.end));
      }
    }
    if (claim.supersedes) node.append(el('p', '上一版本 · ' + claim.supersedes));
    if (claim.review_reason) node.append(el('p', '审核状态 · ' + claim.review_reason));
    for (const vote of claim.review_votes ?? []) node.append(el('p', vote.actor + ' · ' + vote.decision + ' · ' + vote.reason));
    return node;
  }

  async function teamHome(target, choose) {
    const list = await api('team-spaces');
    if (!target.isConnected) return;
    const team = panel('团队空间');
    const detail = el('div');
    for (const item of list) {
      team.append(button([item.label, item.purpose ?? '', item.role ?? ''].filter(Boolean).join(' · '),
        () => teamSpace(detail, item), true));
    }
    if (!list.length) team.append(el('p', '当前没有已授权的团队空间。'));
    const newSpace = receipt('创建空间或更新项目用途', [
      ['id', '空间标识'], ['label', '工作区名称'], ['purpose', '项目用途'],
      ['policy_rev', '当前策略修订，首次创建填 0', 'number', '0'], checkbox('active', '启用此用途', true)
    ], async fields => {
      const result = await api('team-spaces/' + picker(fields.id), { label: fields.label, purpose: fields.purpose,
        policy_rev: Number(fields.policy_rev), active: !!fields.active }, 'PUT');
      await teamHome(target, result);
      return result;
    });
    target.replaceChildren(team, newSpace, detail);
    if (choose) await teamSpace(detail, choose);
  }

  async function teamSpace(target, space) {
    const base = path(space);
    const node = el('div');
    target.replaceChildren(node);
    const heading = panel(space.label);
    heading.append(el('p', [space.space_id, space.purpose, '策略 v' + space.policy_rev, space.role,
      space.can_export ? '可导出当前用途' : '只在线查看'].filter(Boolean).join(' · ')));
    node.append(heading);
    if (!space.purpose) return;
    const search = panel('团队知识搜索');
    const answers = el('div');
    const runSearch = async (values, exporting) => {
      const body = { purpose: space.purpose, query: values.query.trim(), mode: values.hybrid ? 'hybrid' : 'keywords' };
      if (values.as_of) body.as_of = iso(values.as_of);
      const data = await api(base + (exporting ? '/export' : '/search'), body);
      if (exporting) {
        const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }));
        const download = el('a');
        download.href = url;
        download.download = space.space_id + '-knowledge.json';
        download.click();
        setTimeout(() => URL.revokeObjectURL(url), 0);
        return;
      }
      const note = data.refusal ? '没有可用的已审核知识。' :
        '召回 ' + data.hits.length + ' 条 · ' + (data.mode === 'hybrid' ? '关键词与语义' : '关键词');
      answers.replaceChildren(el('p', note), ...data.hits.map(claimCard));
    };
    search.append(form([['query', '查询问题'], ['as_of', '历史时点', 'datetime-local', '', true],
      checkbox('hybrid', '同时使用语义搜索')], '搜索团队知识', values => runSearch(values, false)));
    if (space.can_export) search.append(button('按当前权限下载搜索结果', () =>
      runSearch(Object.fromEntries(new FormData(search.querySelector('form'))), true), true));
    search.append(answers);
    node.append(search);

    if (space.role === 'maintainer') {
      node.append(receipt('成员与导出权限', [['actor', '成员标识'], ['role', '职责（reader、author、reviewer 或 maintainer）', 'text', 'reader'],
        checkbox('can_export', '可以导出当前用途'), checkbox('active', '保留成员访问权', true)], async values => {
        const result = await api(base + '/membership', { actor: values.actor, purpose: space.purpose, role: values.role,
          can_export: !!values.can_export, active: !!values.active, policy_rev: space.policy_rev }, 'PUT');
        space.policy_rev = result.policy_rev;
        heading.querySelector('p').textContent = [space.space_id, space.purpose, '策略 v' + space.policy_rev, space.role].join(' · ');
        return result;
      }));
    }
    if (session.who.role === 'owner' && ['author', 'maintainer'].includes(space.role)) showNomination(node, space);
    if (['author', 'maintainer'].includes(space.role)) {
      node.append(receipt('提交带引证的知识草案', [
        ['key', '请求键', 'text', key()], ['topic', '主题标识'], ['applicability', '适用条件'],
        ['valid_from', '生效时间', 'datetime-local', date(Date.now() - 60000)], ['supersedes', '上一版本编号', 'text', '', true],
        ['nominations', '提名编号和授权版本（JSON）', 'area', '[{"id":"","revision":1}]'],
        ['sentences', '草案句子（JSON）', 'area', '[]'], ['spans', '逐句引证范围（JSON）', 'area', '[]'],
        checkbox('use_model', '允许模型在已授权证据内拟稿')
      ], values => {
        const citations = JSON.parse(values.nominations);
        const body = { purpose: space.purpose, topic: values.topic, applicability: values.applicability,
          valid_from: iso(values.valid_from), nomination_ids: citations.map(cite => cite.id), grant_revisions: citations.map(cite => cite.revision),
          sentences: JSON.parse(values.sentences), evidence_spans: JSON.parse(values.spans), use_model: !!values.use_model };
        if (values.supersedes) body.supersedes = values.supersedes;
        return api(base + '/claims', body, 'POST', keyHeader(values.key));
      }));
    }
    showReview(node, space);
    if (session.who.role === 'owner') showWithdrawal(node);
    if (space.role === 'maintainer') showMaintenance(node, space);
  }

  function showNomination(target, space) {
    const proposal = panel('来源提名预览');
    const output = el('pre');
    output.hidden = true;
    let staged;
    const action = form([['source', '个人来源摘要'], ['start', '起始字节', 'number', '0'],
      ['end', '结束字节'], ['grant_rev', '授权版本', 'number', '1'],
      ['expires_at', '共享截止时间', 'datetime-local', date(Date.now() + 86400000)],
      checkbox('allow_model', '此片段可供本地模型使用')], '预览共享的文字', async values => {
      staged = null;
      const body = { purpose: space.purpose, source: values.source, start: Number(values.start), end: Number(values.end),
        grant_rev: Number(values.grant_rev), expires_at: iso(values.expires_at), allow_model: !!values.allow_model };
      const data = await api(path(space) + '/nominations/preview', body);
      staged = body;
      showReceipt(output, data);
    });
    action.addEventListener('input', () => { staged = null; });
    const personal = receipt('登记个人证据', [['statement', '本人经验原文', 'area']], async values => {
      const statement = values.statement.trim();
      const now = new Date().toISOString();
      const saved = await api('source', { subject: session.who.actor, purpose: 'personal', session: 'team-evidence',
        external_id: key(), occurred: now, kind: 'user', statement });
      action.elements.source.value = saved.source;
      action.elements.end.value = new TextEncoder().encode(statement).length;
      staged = null;
      return saved;
    });
    proposal.append(action, button('确认提交已预览的提名', async () => {
      if (!staged) throw new Error('请先预览当前来源范围。');
      showReceipt(output, await api(path(space) + '/nominations', staged));
    }), output);
    target.append(personal, proposal);
  }

  function showReview(target, space) {
    const area = panel('知识版本与评审');
    const details = el('div');
    const queue = el('div');
    const base = path(space);
    const suffix = purpose(space);
    const reviewer = ['reviewer', 'maintainer'].includes(space.role);
    const show = async id => {
      const current = await api(base + '/claims/' + picker(id) + suffix);
      const card = claimCard(current);
      details.replaceChildren(card);
      if (reviewer && current.review_state === 'awaiting_review') {
        card.append(receipt('提交独立审核', [['decision', '决定（approve、reject 或 dispute）', 'text', 'approve'], ['reason', '审核理由', 'area']],
          async fields => {
            const response = await api(base + '/claims/' + picker(id) + '/votes' + suffix, { ...fields, claim_rev: current.claim_rev });
            await show(id);
            return response;
          }));
      }
      if (space.role === 'maintainer') card.append(receipt('分派复查', [['assignee', '审核人标识']],
        fields => api(base + '/claims/' + picker(id) + '/assignment' + suffix, fields)));
    };
    area.append(form([['id', '知识编号']], '读取结论与逐句出处', fields => show(fields.id)));
    if (reviewer) area.append(button('读取待审、争议和临期项目', async () => {
      const values = await api(base + '/review-queue' + suffix);
      queue.replaceChildren(...values.map(item => button([item.topic, item.reason, item.review_owner].filter(Boolean).join(' · '),
        () => show(item.claim_id), true)));
      if (!values.length) queue.append(el('p', '当前没有待处理项目。'));
    }, true));
    area.append(queue, details);
    target.append(area);
  }

  function showWithdrawal(target) {
    const area = panel('撤回影响与处理进度');
    const output = el('pre');
    output.hidden = true;
    let staged;
    const operation = form([['id', '提名编号'], ['key', '撤回请求键', 'text', key()]], '预览影响条数', async values => {
      const result = await api('team-nominations/' + picker(values.id) + '/impact');
      staged = values;
      showReceipt(output, result);
    });
    operation.addEventListener('input', () => { staged = null; });
    const status = receipt('读取撤回回执', [['id', '撤回操作编号']], values => api('team-withdrawals/' + picker(values.id)));
    area.append(operation, button('撤回已预览的提名', async () => {
      if (!staged) throw new Error('请先预览影响。');
      const result = await api('team-nominations/' + picker(staged.id) + '/withdraw', undefined, 'POST', keyHeader(staged.key));
      status.querySelector('input[name="id"]').value = result.id;
      showReceipt(output, result);
    }), output, status);
    target.append(area);
  }

  function showMaintenance(target, space) {
    const area = panel('检索质量与索引');
    const index = el('pre');
    index.hidden = true;
    const base = path(space);
    const suffix = purpose(space);
    const status = receipt('读取已保存的评测', [['id', '评测编号']], values => api(base + '/evaluations/' + picker(values.id) + suffix));
    area.append(button('清除过时索引并补建一批', async () => showReceipt(index, await api(base + '/reindex' + suffix, {})), true), index,
      receipt('运行冻结问题集', [['cases', '问题集（JSON）', 'area']], async values => {
        const report = await api(base + '/evaluations', { purpose: space.purpose, cases: JSON.parse(values.cases) });
        status.querySelector('input[name="id"]').value = report.evaluation_id;
        return report;
      }), status);
    target.append(area);
  }

  sections.team = ['团队知识', teamHome];
}
