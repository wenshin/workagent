-- 每个空间（个人空间与共享空间）都预设一个「会话」子空间，作为该空间的默认会话入口。
-- 该节点为系统保留：不可改名、不可删除、不可在其下再建子空间。

begin;

-- 1. 为已有活跃空间补建「会话」子空间（已有则跳过）
insert into space (owner_id, name, type, parent_id, path, depth, is_system_reserved)
select s.owner_id, '会话', s.type, s.id, s.path || '/session', s.depth + 1, true
from space s
where s.deleted_at is null
  and s.status = 'active'
  and not s.is_system_reserved
  and not exists (
      select 1
      from space c
      where c.parent_id = s.id
        and c.is_system_reserved
        and c.deleted_at is null
  );

-- 2. 「会话」子空间继承父空间的成员与权限
insert into space_member (space_id, user_id, perm, source)
select c.id, m.user_id, m.perm, 'system'
from space c
join space_member m on m.space_id = c.parent_id
where c.is_system_reserved
  and c.deleted_at is null
  and not exists (
      select 1
      from space_member x
      where x.space_id = c.id and x.user_id = m.user_id
  );

commit;
