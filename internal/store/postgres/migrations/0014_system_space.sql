-- 系统空间：全局唯一、仅系统管理员可见，承载内置的 skills / tools / mcps（Agent 全局能力源）。

-- 1) 放宽空间类型约束：新增 system
alter table space drop constraint if exists space_type_check;
alter table space add constraint space_type_check check (type in ('personal', 'shared', 'system'));

-- 2) 系统空间全局唯一（未删除范围内）
create unique index if not exists space_system_uniq
    on space ((type))
    where type = 'system' and deleted_at is null;
