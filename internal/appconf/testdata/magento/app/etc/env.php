<?php
return [
    'backend' => [
        'frontName' => 'admin_1x2y3z'
    ],
    'crypt' => [
        'key' => '00000000000000000000000000000000'
    ],
    'db' => [
        'table_prefix' => '',
        'connection' => [
            'default' => [
                'host' => '127.0.0.1:3308',
                'dbname' => 'magento',
                'username' => 'magento',
                'password' => 'S3cr\'magento',
                'model' => 'mysql4',
                'engine' => 'innodb',
                'initStatements' => 'SET NAMES utf8;',
                'active' => '1',
                'driver_options' => [
                    1014 => false
                ]
            ]
        ]
    ],
    'resource' => [
        'default_setup' => [
            'connection' => 'default'
        ]
    ],
    'x-frame-options' => 'SAMEORIGIN',
    'MAGE_MODE' => 'developer',
    'session' => [
        'save' => 'files'
    ],
    'cache_types' => [
        'config' => 1,
        'layout' => 1
    ],
    'install' => [
        'date' => 'Thu, 01 Jan 2026 00:00:00 +0000'
    ]
];
