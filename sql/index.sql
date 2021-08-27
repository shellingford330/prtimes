USE isuconp;
ALTER TABLE `comments` ADD INDEX post_id_created_at_idx (`post_id`, `created_at`);
