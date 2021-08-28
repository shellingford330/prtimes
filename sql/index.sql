USE isuconp;
ALTER TABLE `comments` ADD INDEX post_id_created_at_idx (`post_id`, `created_at`);

ALTER TABLE `posts` ADD COLUMN user_del_flg tinyint(1) NOT NULL DEFAULT 0;
UPDATE `posts` INNER JOIN `users` ON `posts`.`user_id` = `users`.`id` SET `posts`.`user_del_flg` = `users`.`del_flg`;

-- 効果はあまりないので外してもいいかも
ALTER TABLE `posts` ADD INDEX user_del_flg_created_at_idx (`user_del_flg`, `created_at`);
