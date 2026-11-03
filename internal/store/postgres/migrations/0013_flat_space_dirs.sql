-- 空间只有一级：取消子空间概念，第二层起都是普通目录（可多级）。
-- 会话可挂在空间内的某个目录下（dir 为空表示直接属于空间根）。

-- 1) 会话新增 dir：相对 space/files/ 的目录路径，例如 "设计" 或 "技能A/子目录"
alter table session add column if not exists dir text;

-- 2) 历史子空间的会话上移到根空间，避免随子空间软删一起消失
with recursive up as (
    select s.id as space_id, s.id as node_id, s.parent_id
    from space s
    where s.deleted_at is null
    union all
    select up.space_id, p.id, p.parent_id
    from up
    join space p on p.id = up.parent_id
    where p.deleted_at is null
),
roots as (
    select space_id, node_id as root_id from up where parent_id is null
)
update session s
set space_id = r.root_id, updated_at = now()
from roots r
where s.space_id = r.space_id
  and r.space_id <> r.root_id;

-- 3) 软删所有子空间（保留历史行，避免外键悬挂）
update space
set deleted_at = now(), status = 'deleted', updated_at = now()
where parent_id is not null
  and deleted_at is null;

-- 4) 目录内会话按 (space_id, dir) 检索
create index if not exists session_space_dir_idx
    on session (space_id, dir)
    where status = 'active';
