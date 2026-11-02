-- 0004 只为「没有任何会话」的空间补了默认会话，已有会话的空间会缺少「新会话」入口。
-- 这里为每个活跃空间补齐一个标题仍可自动生成的「新会话」。

begin;

insert into session (space_id, title, created_by, title_auto)
select s.id, '新会话', s.owner_id, true
from space s
where s.deleted_at is null
  and s.status = 'active'
  and not exists (
      select 1
      from session x
      where x.space_id = s.id
        and x.status = 'active'
        and x.title = '新会话'
        and x.title_auto
  );

commit;
