-- ============================================================
-- TBDS 管控平台建表 SQL
-- 数据库: woodpecker
-- 表总数: 6 (job, stage, task, action, host, cluster)
-- ============================================================

-- 使用数据库
CREATE DATABASE IF NOT EXISTS woodpecker DEFAULT CHARACTER SET utf8mb4;
USE woodpecker;

-- -----------------------------------------------------------
-- 1. Job 表：一个完整的操作流程
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `job` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `job_name`        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Job 名称',
    `job_code`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Job 代码（如 INSTALL_YARN）',
    `process_code`    VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程代码',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=running, 2=success, -1=failed, -2=cancelled',
    `synced`          INT(11)       NOT NULL DEFAULT 0 COMMENT '0=未同步, 1=已同步到 TaskCenter',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度 0.0~1.0',
    `request`         TEXT          COMMENT '请求参数 JSON',
    `result`          TEXT          COMMENT '执行结果 JSON',
    `error_msg`       TEXT          COMMENT '错误信息',
    `operator`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '操作人',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`         DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_job_cluster` (`cluster_id`),
    KEY `idx_job_state` (`state`),
    KEY `idx_job_process` (`process_id`),
    KEY `idx_job_synced` (`synced`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Job 操作任务表';

-- -----------------------------------------------------------
-- 2. Stage 表：操作的一个阶段（Stage 之间顺序执行）
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `stage` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `stage_id`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Stage 唯一标识',
    `stage_name`      VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Stage 名称',
    `stage_code`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Stage 代码',
    `job_id`          BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `process_code`    VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程代码',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=running, 2=success, -1=failed',
    `order_num`       INT(11)       NOT NULL DEFAULT 0 COMMENT '执行顺序（从 0 开始）',
    `is_last_stage`   TINYINT(1)    NOT NULL DEFAULT 0 COMMENT '是否为最后一个 Stage',
    `next_stage_id`   VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '下一个 Stage ID（链表驱动）',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度',
    `error_msg`       TEXT          COMMENT '错误信息',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`         DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_stage_id` (`stage_id`),
    KEY `idx_stage_job` (`job_id`),
    KEY `idx_stage_process` (`process_id`),
    KEY `idx_stage_state` (`state`),
    KEY `idx_stage_cluster` (`cluster_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Stage 操作阶段表';

-- -----------------------------------------------------------
-- 3. Task 表：阶段内的子任务（同一 Stage 内并行执行）
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `task` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `task_id`         VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Task 唯一标识',
    `task_name`       VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Task 名称',
    `task_code`       VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Task 代码',
    `stage_id`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属 Stage ID',
    `job_id`          BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=inProcess, 2=success, -1=failed',
    `action_num`      INT(11)       NOT NULL DEFAULT 0 COMMENT 'Action 总数',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度',
    `retry_count`     INT(11)       NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `retry_limit`     INT(11)       NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `error_msg`       TEXT          COMMENT '错误信息',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`         DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_task_stage` (`stage_id`),
    KEY `idx_task_job` (`job_id`),
    KEY `idx_task_state` (`state`),
    KEY `idx_task_process` (`process_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Task 子任务表';

-- -----------------------------------------------------------
-- 4. Action 表：每个节点具体执行的 Shell 命令
-- ⚠️ 整个系统中数据量最大、读写最频繁的表
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `action` (
    `id`                    BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `action_id`             VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Action 唯一标识',
    `task_id`               BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Task ID',
    `stage_id`              VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属 Stage ID',
    `job_id`                BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `cluster_id`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `hostuuid`              VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '目标节点 UUID',
    `ipv4`                  VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '目标节点 IP',
    `commond_code`          VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '命令代码',
    `command_json`          TEXT          COMMENT '命令 JSON（Shell 命令详情）',
    `action_type`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=Agent, 1=Bootstrap',
    `state`                 INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=cached, 2=executing, 3=success, -1=failed, -2=timeout',
    `exit_code`             INT(11)       NOT NULL DEFAULT 0 COMMENT '退出码',
    `result_state`          INT(11)       NOT NULL DEFAULT 0 COMMENT '结果状态',
    `stdout`                TEXT          COMMENT '标准输出',
    `stderr`                TEXT          COMMENT '标准错误',
    `dependent_action_id`   BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '依赖的 Action ID（0=无依赖）',
    `serial_flag`           VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '串行执行标志',
    `createtime`            DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`            DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`               DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_action_id` (`action_id`),
    KEY `idx_action_state` (`state`),
    KEY `idx_action_task` (`task_id`),
    KEY `idx_action_stage` (`stage_id`),
    KEY `idx_action_host` (`hostuuid`),
    KEY `idx_action_job` (`job_id`),
    KEY `idx_action_cluster` (`cluster_id`),
    KEY `idx_action_dependent` (`dependent_action_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Action 节点命令表';

-- -----------------------------------------------------------
-- 5. Host 表：被管控的节点信息
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `host` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `uuid`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '节点唯一标识',
    `hostname`        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '主机名',
    `ipv4`            VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'IPv4 地址',
    `ipv6`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'IPv6 地址',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属集群 ID',
    `status`          INT(11)       NOT NULL DEFAULT 0 COMMENT '0=offline, 1=online',
    `disk_total`      BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '磁盘总量（字节）',
    `disk_free`       BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '磁盘可用（字节）',
    `mem_total`       BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '内存总量（字节）',
    `mem_free`        BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '内存可用（字节）',
    `cpu_usage`       FLOAT         NOT NULL DEFAULT 0 COMMENT 'CPU 使用率',
    `last_heartbeat`  DATETIME      DEFAULT NULL COMMENT '最后心跳时间',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_host_uuid` (`uuid`),
    KEY `idx_host_cluster` (`cluster_id`),
    KEY `idx_host_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='节点信息表';

-- -----------------------------------------------------------
-- 6. Cluster 表：集群信息
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `cluster` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群唯一标识',
    `cluster_name`    VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '集群名称',
    `cluster_type`    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '集群类型（EMR/LIGHTNESS）',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '状态',
    `node_count`      INT(11)       NOT NULL DEFAULT 0 COMMENT '节点数量',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_cluster_id` (`cluster_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='集群信息表';

-- -----------------------------------------------------------
-- 测试数据：插入一个测试集群和 3 个测试节点
-- （便于 Step 2 开始时直接创建 Job）
-- -----------------------------------------------------------
INSERT INTO `cluster` (`cluster_id`, `cluster_name`, `cluster_type`, `state`, `node_count`)
VALUES ('cluster-001', '测试集群', 'EMR', 1, 3)
ON DUPLICATE KEY UPDATE `cluster_name` = VALUES(`cluster_name`);

INSERT INTO `host` (`uuid`, `hostname`, `ipv4`, `cluster_id`, `status`)
VALUES
    ('node-001', 'tbds-node-01', '10.0.0.1', 'cluster-001', 1),
    ('node-002', 'tbds-node-02', '10.0.0.2', 'cluster-001', 1),
    ('node-003', 'tbds-node-03', '10.0.0.3', 'cluster-001', 1)
ON DUPLICATE KEY UPDATE `hostname` = VALUES(`hostname`);
