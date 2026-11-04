-- 智能体绑定：会话创建时选定 agents/ 里的一个配置，并把追加提示词固化在本行。
-- 0003 里预留的 agent_id 是 uuid，但从未被使用；agents/ 用可读 key（如 base），
-- 因此这里把它改成 text。
alter table session
    alter column agent_id type text using agent_id::text;

-- agent_version 记录创建时的版本，配置被改动后能看出漂移
alter table session add column if not exists agent_version text not null default '';

-- agent_prompt 是创建时固化的追加提示词：管理员事后改 agents/ 不影响已开会话
alter table session add column if not exists agent_prompt text not null default '';

create index if not exists session_agent_idx on session (agent_id) where agent_id is not null;
