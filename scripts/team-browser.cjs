'use strict';

const assert = require('node:assert/strict');
const { chromium } = require(process.env.MEMORY_TEST_PLAYWRIGHT || 'playwright');

async function enter(page, token) {
  await page.goto(process.env.MEMORY_BROWSER_ORIGIN);
  await page.getByLabel('访问令牌').fill(token);
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('button', { name: '团队知识', exact: true }).click();
  await page.getByRole('button', { name: /Shop floor · operations/ }).click();
}

async function run() {
  const browser = await chromium.launch({ executablePath: process.env.MEMORY_TEST_CHROME, headless: true });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await enter(page, process.env.MEMORY_BROWSER_READER);
    await page.getByLabel('查询问题').fill('Starlight crates');
    await page.getByRole('button', { name: '搜索团队知识', exact: true }).click();
    await page.getByRole('heading', { name: 'Starlight crates are inspected in the north loading bay.' }).waitFor();
    await page.locator('blockquote').filter({ hasText: 'north loading bay' }).waitFor();
    assert.equal(await page.getByRole('heading', { name: '提交带引证的知识草案' }).count(), 0);
    const exportResponse = page.waitForResponse(r => r.url().endsWith('/team-spaces/shop-floor/export'));
    const download = page.waitForEvent('download');
    await page.getByRole('button', { name: '按当前权限下载搜索结果' }).click();
    const response = await exportResponse;
    assert.equal(response.status(), 200);
    assert.equal((await response.json()).hits[0].claim_id, process.env.MEMORY_BROWSER_CLAIM);
    assert.equal((await download).suggestedFilename(), 'shop-floor-knowledge.json');

    await enter(page, process.env.MEMORY_BROWSER_OWNER);
    await page.getByLabel('知识编号', { exact: true }).fill(process.env.MEMORY_BROWSER_CLAIM);
    await page.getByRole('button', { name: '读取结论与逐句出处', exact: true }).click();
    await page.getByText('jamie · approve · Compared with the nominated evidence', { exact: true }).waitFor();
    await page.getByText('taylor · approve · Compared with the nominated evidence', { exact: true }).waitFor();
    await page.getByRole('heading', { name: '检索质量与索引', exact: true }).waitFor();
    assert.deepEqual(errors, []);
    process.stdout.write(JSON.stringify({ chromium: browser.version(), checks: [
      'search-current-scope', 'sentence-and-quote', 'policy-export', 'reader-has-no-draft-form', 'two-reviewer-receipts', 'maintenance-console'
    ] }));
  } finally {
    await browser.close();
  }
}

run().catch(error => {
  console.error(error.message);
  process.exitCode = 1;
});
