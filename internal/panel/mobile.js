/* 移动端 A 方案：只负责导航与详情呈现，真实 API 操作继续走 app.js 的账号动作。
   在 app.js 之前执行，暴露 MobilePanel 供 go()/loadOverview() 同步状态。 */
'use strict';
const MobilePanel = (() => {
  const $ = id => document.getElementById(id);
  const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  let uid = null;
  let lastStatus = null;
  const mobile = () => matchMedia('(max-width: 760px)').matches;

  function coolSeconds(s) {
    return Math.max(s.cool_remaining_sec || 0,
      (new Date(s.breaker_until || 0) - Date.now()) / 1000,
      (new Date(s.degrade_until || 0) - Date.now()) / 1000);
  }
  function statusText(s) {
    if (s.disabled) return ['已禁用', 'bad'];
    if (s.paused) return ['已暂停选号', 'warn'];
    if (coolSeconds(s) > 0) return ['冷却中', 'warn'];
    if (s.session_dead_fails > 0) return ['疑似登录失效', 'warn'];
    return ['可用', 'ok'];
  }
  function token(n) {
    if (n == null) return '—';
    if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return String(n);
  }
  function action(uidValue, name, label, primary = false, danger = false) {
    return '<button data-a="' + name + '" data-u="' + esc(uidValue) + '"' +
      (primary ? ' class="primary"' : '') + (danger ? ' class="danger"' : '') + '>' + label + '</button>';
  }
  function detailActions(s) {
    const u = s.uid;
    const out = [];
    if (!s.enterprise && !s.api_key) out.push(action(u, 'checkin', s.checkin_done ? '重新签到' : '立即签到', true));
    out.push(action(u, 'balance', s.enterprise || s.api_key ? '刷新额度' : '刷新余额'));
    out.push(action(u, 'models', '模型开关'));
    if (!s.enterprise && !s.api_key) out.push(action(u, 'tasks', '账号任务'));
    if (!s.enterprise && !s.api_key && s.realm !== 'global') out.push(action(u, 'travel', '旅行巡检'));
    if (s.disabled || coolSeconds(s) > 0) out.push(action(u, 'revive', '解冻账号', true));
    else if (s.paused) out.push(action(u, 'resume', '恢复选号', true));
    else out.push(action(u, 'pause', '暂停选号'));
    if (!s.disabled) out.push(action(u, 'disable', '禁用账号', false, true));
    out.push(action(u, 'remove', '移除账号', false, true));
    return out.join('');
  }
  function renderDetail(s) {
    if (!s) return;
    const [state, cls] = statusText(s);
    const name = s.nickname || '未命名账号';
    const initial = (s.nickname || 'WB').trim().slice(0, 1).toUpperCase();
    const realm = s.realm === 'global' ? '国际版' : '国内版';
    const unlimited = s.credits_total === -1;
    const credit = unlimited ? '不限' : (s.credits == null ? '—' : Number(s.credits).toLocaleString());
    const total = unlimited ? '' : (s.credits_total > 0 ? ' / ' + Number(s.credits_total).toLocaleString() : '');
    const usage = s.token_usage || {};
    $('mobileDetailBody').innerHTML =
      '<section class="mobile-account-hero">' +
        '<div class="mobile-avatar">' + esc(initial) + '</div>' +
        '<h2 id="mobileDetailName">' + esc(name) + '</h2>' +
        '<div class="mobile-uid">' + esc(s.uid) + ' · ' + realm + '</div>' +
        '<span class="tag ' + cls + '">' + state + '</span>' +
        '<div class="mobile-credit">' + credit + '<small>剩余积分' + total + '</small></div>' +
      '</section>' +
      '<section class="mobile-facts">' +
        '<div><span>成功 / 失败</span><b>' + (s.success_count || 0) + ' / ' + (s.err_total || 0) + '</b></div>' +
        '<div><span>当前在途</span><b>' + (s.in_flight || 0) + '</b></div>' +
        '<div><span>最近请求</span><b>' + (usage.request_count || 0) + ' 次</b></div>' +
        '<div><span>累计 Token</span><b>' + token(usage.total_tokens) + '</b></div>' +
        '<div><span>手动关闭模型</span><b>' + ((s.model_off || []).length) + ' 个</b></div>' +
        '<div><span>最近成功</span><b>' + esc(s.last_success ? new Date(s.last_success).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '—') + '</b></div>' +
      '</section>' +
      '<section class="mobile-account-actions">' + detailActions(s) + '</section>';
  }

  function syncNav(view) {
    const current = view === 'more' ? 'more' : view;
    document.querySelectorAll('#mobileNav [data-view]').forEach(b => b.classList.toggle('on', b.dataset.view === current));
    document.body.classList.toggle('mobile-more', view === 'more');
    document.body.classList.toggle('mobile-accounts', view === 'accounts');
    document.body.classList.toggle('mobile-detail-open', !!uid);
  }
  function open(uidValue) {
    if (!mobile()) return;
    uid = uidValue;
    const s = lastStatus && lastStatus.accounts && lastStatus.accounts.find(x => x.uid === uid);
    if (s) renderDetail(s);
    $('mobileAccountDetail').hidden = false;
    document.body.classList.add('mobile-detail-open');
    $('mobileDetailBack').focus();
  }
  function close() {
    uid = null;
    $('mobileAccountDetail').hidden = true;
    document.body.classList.remove('mobile-detail-open');
  }
  function update(data) {
    lastStatus = data;
    if (!uid || !data || !data.accounts) return;
    const s = data.accounts.find(x => x.uid === uid);
    if (s) renderDetail(s);
    else close(); // 删除账号后详情不再悬挂。
  }

  document.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('#mobileNav [data-view]').forEach(b => b.addEventListener('click', () => {
      close();
      if (b.dataset.view === 'more') {
        go('more');
        history.replaceState(null, '', '#more');
      } else {
        go(b.dataset.view);
        history.replaceState(null, '', '#' + b.dataset.view);
      }
    }));
    $('mobileDetailBack').addEventListener('click', close);
    $('mobileDetailClose').addEventListener('click', close);
    document.addEventListener('keydown', e => { if (e.key === 'Escape' && uid) close(); });
    matchMedia('(max-width: 760px)').addEventListener('change', e => {
      close();
      if (view === 'more') go(e.matches ? 'more' : 'accounts');
      else syncNav(view);
    });
  });

  return { syncNav, open, close, update, mobile };
})();
